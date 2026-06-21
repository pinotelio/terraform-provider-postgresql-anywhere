package postgresql

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	sdkschema "github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/lib/pq"
)

var (
	_ resource.Resource                = (*grantResource)(nil)
	_ resource.ResourceWithConfigure   = (*grantResource)(nil)
	_ resource.ResourceWithImportState = (*grantResource)(nil)
)

// grantAllowedObjectTypes is the set of object_type values supported by the
// postgresql_grant resource.
var grantAllowedObjectTypes = []string{
	"database",
	"function",
	"procedure",
	"routine",
	"schema",
	"sequence",
	"table",
	"foreign_data_wrapper",
	"foreign_server",
	"column",
}

// objectTypes maps an object_type to its pg_class relkind / defacl objtype.
// resource_default_privileges_fw.go also references this map, so its name must
// not change.
var objectTypes = map[string]string{
	"table":    "r",
	"sequence": "S",
	"function": "f",
	"routine":  "f",
	"type":     "T",
	"schema":   "n",
}

// NewGrantResource returns the postgresql_grant resource.
func NewGrantResource() resource.Resource {
	return &grantResource{}
}

type grantResource struct {
	client *Client
}

type grantResourceModel struct {
	ID              types.String `tfsdk:"id"`
	Role            types.String `tfsdk:"role"`
	Database        types.String `tfsdk:"database"`
	Schema          types.String `tfsdk:"schema"`
	ObjectType      types.String `tfsdk:"object_type"`
	Objects         types.Set    `tfsdk:"objects"`
	Columns         types.Set    `tfsdk:"columns"`
	Privileges      types.Set    `tfsdk:"privileges"`
	WithGrantOption types.Bool   `tfsdk:"with_grant_option"`
}

// grantFields carries the resolved values used to render GRANT/REVOKE
// statements. objects/columns/privileges are sets so that ordering and
// identifier rendering are deterministic.
type grantFields struct {
	role            string
	database        string
	schemaName      string
	objectType      string
	objects         *sdkschema.Set
	columns         *sdkschema.Set
	privileges      *sdkschema.Set
	withGrantOption bool
}

func (r *grantResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_grant"
}

func (r *grantResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages the privileges granted to a PostgreSQL role on database objects.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Synthetic id built from role, database, schema, object_type, objects and columns",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"role": schema.StringAttribute{
				Required:      true,
				Description:   "The name of the role to grant privileges on",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"database": schema.StringAttribute{
				Required:      true,
				Description:   "The database to grant privileges on for this role",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"schema": schema.StringAttribute{
				Optional:      true,
				Description:   "The database schema to grant privileges on for this role",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"object_type": schema.StringAttribute{
				Required:    true,
				Description: "The PostgreSQL object type to grant the privileges on (one of: " + strings.Join(grantAllowedObjectTypes, ", ") + ")",
				Validators: []validator.String{
					stringvalidator.OneOf(grantAllowedObjectTypes...),
				},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"objects": schema.SetAttribute{
				ElementType:   types.StringType,
				Optional:      true,
				Description:   "The specific objects to grant privileges on for this role (empty means all objects of the requested type)",
				PlanModifiers: []planmodifier.Set{setplanmodifier.RequiresReplace()},
			},
			"columns": schema.SetAttribute{
				ElementType:   types.StringType,
				Optional:      true,
				Description:   "The specific columns to grant privileges on for this role",
				PlanModifiers: []planmodifier.Set{setplanmodifier.RequiresReplace()},
			},
			"privileges": schema.SetAttribute{
				ElementType: types.StringType,
				Required:    true,
				Description: "The list of privileges to grant",
			},
			"with_grant_option": schema.BoolAttribute{
				Optional:      true,
				Computed:      true,
				Default:       booldefault.StaticBool(false),
				Description:   "Permit the grant recipient to grant it to others",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},
		},
	}
}

func (r *grantResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected provider data",
			fmt.Sprintf("Expected *postgresql.Client, got %T. This is a provider bug.", req.ProviderData),
		)
		return
	}
	r.client = client
}

// ImportState reports that import is unsupported: the grant's composite, lossy
// id cannot be unambiguously parsed back into configuration.
func (r *grantResource) ImportState(_ context.Context, _ resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.AddError(
		"Import not supported",
		"the postgresql_grant resource does not support import",
	)
}

func (r *grantResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan grantResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.createOrUpdate(ctx, &plan, nil, &resp.Diagnostics, &resp.State)
}

func (r *grantResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state grantResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.createOrUpdate(ctx, &plan, &state, &resp.Diagnostics, &resp.State)
}

// createOrUpdate applies the grant. When prev is non-nil (update), the REVOKE
// uses the previously granted privileges (everything else is RequiresReplace, so
// only privileges can differ).
func (r *grantResource) createOrUpdate(ctx context.Context, plan *grantResourceModel, prev *grantResourceModel, diags *diag.Diagnostics, state *tfsdk.State) {
	db, err := r.client.Connect()
	if err != nil {
		diags.AddError("could not connect to database", err.Error())
		return
	}

	objectType := plan.ObjectType.ValueString()
	if err := validateFeatureSupportFW(db, objectType); err != nil {
		diags.AddError("feature is not supported", err.Error())
		return
	}

	fields, d := r.grantFieldsFromModel(ctx, plan)
	diags.Append(d...)
	if diags.HasError() {
		return
	}

	// Validate parameters.
	// NOTE: the message is placed in the diagnostic SUMMARY (not the detail).
	// Terraform word-wraps the detail when rendering, which would insert newlines
	// into long messages and break acceptance-test ExpectError regexps that span
	// the wrap point. Summaries are not wrapped.
	if plan.Schema.ValueString() == "" && !sliceContainsStr([]string{"database", "foreign_data_wrapper", "foreign_server"}, objectType) {
		diags.AddError("parameter 'schema' is mandatory for postgresql_grant resource", "invalid postgresql_grant configuration")
		return
	}
	if fields.objects.Len() > 0 && (objectType == "database" || objectType == "schema") {
		diags.AddError("cannot specify `objects` when `object_type` is `database` or `schema`", "invalid postgresql_grant configuration")
		return
	}
	if fields.columns.Len() > 0 && objectType != "column" {
		diags.AddError("cannot specify `columns` when `object_type` is not `column`", "invalid postgresql_grant configuration")
		return
	}
	if fields.columns.Len() == 0 && objectType == "column" {
		diags.AddError("must specify `columns` when `object_type` is `column`", "invalid postgresql_grant configuration")
		return
	}
	if fields.privileges.Len() != 1 && objectType == "column" {
		diags.AddError("must specify exactly 1 `privileges` when `object_type` is `column`", "invalid postgresql_grant configuration")
		return
	}
	if fields.objects.Len() != 1 && objectType == "column" {
		diags.AddError("must specify exactly 1 table in the `objects` field when `object_type` is `column`", "invalid postgresql_grant configuration")
		return
	}
	if fields.objects.Len() != 1 && (objectType == "foreign_data_wrapper" || objectType == "foreign_server") {
		diags.AddError("one element must be specified in `objects` when `object_type` is `foreign_data_wrapper` or `foreign_server`", "invalid postgresql_grant configuration")
		return
	}
	if err := validateGrantPrivileges(db, objectType, fields.privileges); err != nil {
		diags.AddError("invalid privileges", err.Error())
		return
	}

	database := plan.Database.ValueString()

	txn, err := startTransaction(r.client, database)
	if err != nil {
		diags.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	role := plan.Role.ValueString()
	if err := pgLockRole(txn, role); err != nil {
		diags.AddError("could not lock role", err.Error())
		return
	}

	if objectType == "database" {
		if err := pgLockDatabase(txn, database); err != nil {
			diags.AddError("could not lock database", err.Error())
			return
		}
	}

	owners, err := getRolesToGrantFW(txn, objectType, plan.Schema.ValueString())
	if err != nil {
		diags.AddError("could not determine roles to grant", err.Error())
		return
	}

	// Build the REVOKE fields: identical to the GRANT fields except that, on an
	// update, we revoke the previously granted privileges.
	revokeFields := fields
	if prev != nil {
		prevPrivileges, dd := fwStringSetToSDK(ctx, prev.Privileges)
		diags.Append(dd...)
		if diags.HasError() {
			return
		}
		revokeFields.privileges = prevPrivileges
	}

	if err := withRolesGranted(txn, owners, func() error {
		// Revoke all privileges before granting otherwise reducing privileges
		// will not work. We just have to revoke them in the same transaction so
		// the role will not lose its privileges between the revoke and grant.
		if err := revokeRolePrivilegesFW(txn, revokeFields); err != nil {
			return err
		}
		return grantRolePrivilegesFW(txn, fields)
	}); err != nil {
		diags.AddError("could not apply grant", err.Error())
		return
	}

	if err := txn.Commit(); err != nil {
		diags.AddError("could not commit transaction", err.Error())
		return
	}

	plan.ID = types.StringValue(generateGrantIDFW(fields))

	txn2, err := startTransaction(r.client, database)
	if err != nil {
		diags.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn2)

	diags.Append(r.readRolePrivilegesFW(ctx, txn2, db, plan)...)
	if diags.HasError() {
		return
	}

	diags.Append(state.Set(ctx, plan)...)
}

func (r *grantResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data grantResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	db, err := r.client.Connect()
	if err != nil {
		resp.Diagnostics.AddError("could not connect to database", err.Error())
		return
	}

	objectType := data.ObjectType.ValueString()
	if err := validateFeatureSupportFW(db, objectType); err != nil {
		resp.Diagnostics.AddError("feature is not supported", err.Error())
		return
	}

	exists, err := checkRoleDBSchemaExistsFW(db, data.Role.ValueString(), data.Database.ValueString(), data.Schema.ValueString(), objectType)
	if err != nil {
		resp.Diagnostics.AddError("could not check prerequisites", err.Error())
		return
	}
	if !exists {
		resp.State.RemoveResource(ctx)
		return
	}

	fields, d := r.grantFieldsFromModel(ctx, &data)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	data.ID = types.StringValue(generateGrantIDFW(fields))

	txn, err := startTransaction(r.client, data.Database.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	resp.Diagnostics.Append(r.readRolePrivilegesFW(ctx, txn, db, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *grantResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data grantResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	db, err := r.client.Connect()
	if err != nil {
		resp.Diagnostics.AddError("could not connect to database", err.Error())
		return
	}

	objectType := data.ObjectType.ValueString()
	if err := validateFeatureSupportFW(db, objectType); err != nil {
		resp.Diagnostics.AddError("feature is not supported", err.Error())
		return
	}

	fields, d := r.grantFieldsFromModel(ctx, &data)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}

	database := data.Database.ValueString()
	txn, err := startTransaction(r.client, database)
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	role := data.Role.ValueString()
	if err := pgLockRole(txn, role); err != nil {
		resp.Diagnostics.AddError("could not lock role", err.Error())
		return
	}

	if objectType == "database" {
		if err := pgLockDatabase(txn, database); err != nil {
			resp.Diagnostics.AddError("could not lock database", err.Error())
			return
		}
	}

	owners, err := getRolesToGrantFW(txn, objectType, data.Schema.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("could not determine roles to grant", err.Error())
		return
	}

	if err := withRolesGranted(txn, owners, func() error {
		return revokeRolePrivilegesFW(txn, fields)
	}); err != nil {
		resp.Diagnostics.AddError("could not revoke privileges", err.Error())
		return
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("could not commit transaction", err.Error())
		return
	}
}

// grantFieldsFromModel converts a model into grantFields, materializing the sets
// used for SQL rendering.
func (r *grantResource) grantFieldsFromModel(ctx context.Context, m *grantResourceModel) (grantFields, diag.Diagnostics) {
	var diags diag.Diagnostics

	objects, d := fwStringSetToSDK(ctx, m.Objects)
	diags.Append(d...)
	columns, d := fwStringSetToSDK(ctx, m.Columns)
	diags.Append(d...)
	privileges, d := fwStringSetToSDK(ctx, m.Privileges)
	diags.Append(d...)

	return grantFields{
		role:            m.Role.ValueString(),
		database:        m.Database.ValueString(),
		schemaName:      m.Schema.ValueString(),
		objectType:      m.ObjectType.ValueString(),
		objects:         objects,
		columns:         columns,
		privileges:      privileges,
		withGrantOption: m.WithGrantOption.ValueBool(),
	}, diags
}

// readRolePrivilegesFW reads the live privileges, mutating data.Privileges (and
// data.Columns for column grants) when the live state drifts from the configured
// value.
func (r *grantResource) readRolePrivilegesFW(ctx context.Context, txn *sql.Tx, db *DBConnection, data *grantResourceModel) diag.Diagnostics {
	var diags diag.Diagnostics

	fields, d := r.grantFieldsFromModel(ctx, data)
	diags.Append(d...)
	if diags.HasError() {
		return diags
	}

	roleName := fields.role
	objectType := fields.objectType
	objects := fields.objects

	roleOID, err := getRoleOID(txn, roleName)
	if err != nil {
		diags.AddError("could not get role OID", err.Error())
		return diags
	}

	switch objectType {
	case "database":
		return r.readPrivilegesFromQuery(ctx, txn, db, data, fields, `
SELECT array_agg(privilege_type)
FROM (
	SELECT (aclexplode(datacl)).* FROM pg_database WHERE datname=$1
) as privileges
WHERE grantee = $2
`, []any{fields.database, roleOID}, "database "+fields.database)

	case "schema":
		return r.readPrivilegesFromQuery(ctx, txn, db, data, fields, `
SELECT array_agg(privilege_type)
FROM (
	SELECT (aclexplode(nspacl)).* FROM pg_namespace WHERE nspname=$1
) as privileges
WHERE grantee = $2
`, []any{fields.schemaName, roleOID}, "schema "+fields.schemaName)

	case "foreign_data_wrapper":
		fdwName := objects.List()[0].(string)
		return r.readPrivilegesFromQuery(ctx, txn, db, data, fields, `
SELECT pg_catalog.array_agg(privilege_type)
FROM (
	SELECT (pg_catalog.aclexplode(fdwacl)).* FROM pg_catalog.pg_foreign_data_wrapper WHERE fdwname=$1
) as privileges
WHERE grantee = $2
`, []any{fdwName, roleOID}, "foreign data wrapper "+fdwName)

	case "foreign_server":
		srvName := objects.List()[0].(string)
		return r.readPrivilegesFromQuery(ctx, txn, db, data, fields, `
SELECT pg_catalog.array_agg(privilege_type)
FROM (
	SELECT (pg_catalog.aclexplode(srvacl)).* FROM pg_catalog.pg_foreign_server WHERE srvname=$1
) as privileges
WHERE grantee = $2
`, []any{srvName, roleOID}, "foreign server "+srvName)

	case "column":
		return r.readColumnRolePrivilegesFW(ctx, txn, data, fields)
	}

	// function/procedure/routine and the default (table/sequence/...) branches
	// iterate over every matching object.
	var query string
	var rows *sql.Rows
	switch objectType {
	case "function", "procedure", "routine":
		query = `
SELECT pg_proc.proname, array_remove(array_agg(privilege_type), NULL)
FROM pg_proc
JOIN pg_namespace ON pg_namespace.oid = pg_proc.pronamespace
LEFT JOIN (
    select acls.*
    from (
             SELECT proname, pronamespace, (aclexplode(proacl)).* FROM pg_proc
         ) acls
    WHERE grantee = $1
) privs
USING (proname, pronamespace)
      WHERE nspname = $2
GROUP BY pg_proc.proname
`
		rows, err = txn.Query(query, roleOID, fields.schemaName)
	default:
		query = `
SELECT pg_class.relname, array_remove(array_agg(privilege_type), NULL)
FROM pg_class
JOIN pg_namespace ON pg_namespace.oid = pg_class.relnamespace
LEFT JOIN (
    SELECT acls.* FROM (
        SELECT relname, relnamespace, relkind, (aclexplode(relacl)).* FROM pg_class c
    ) as acls
    WHERE grantee=$1
) privs
USING (relname, relnamespace, relkind)
WHERE nspname = $2 AND relkind = $3
GROUP BY pg_class.relname
`
		rows, err = txn.Query(query, roleOID, fields.schemaName, objectTypes[objectType])
	}
	if err != nil {
		diags.AddError("could not read privileges", err.Error())
		return diags
	}
	defer rows.Close()

	for rows.Next() {
		var objName string
		var privileges pq.ByteaArray
		if err := rows.Scan(&objName, &privileges); err != nil {
			diags.AddError("could not scan privileges", err.Error())
			return diags
		}

		if objects.Len() > 0 && !objects.Contains(objName) {
			continue
		}

		privilegesSet := pgArrayToSet(privileges)
		if !grantResourcePrivilegesEqual(privilegesSet, fields.privileges, objectType, db) {
			log.Printf(
				"[DEBUG] %s %s has not the expected privileges %v for role %s",
				strings.ToTitle(objectType), objName, privileges, roleName,
			)
			pset, dd := sdkSetToFWSet(ctx, privilegesSet)
			diags.Append(dd...)
			if diags.HasError() {
				return diags
			}
			data.Privileges = pset
			break
		}
	}

	return diags
}

// readPrivilegesFromQuery handles the single-row read paths (database, schema,
// foreign data wrapper, foreign server).
func (r *grantResource) readPrivilegesFromQuery(ctx context.Context, txn *sql.Tx, db *DBConnection, data *grantResourceModel, fields grantFields, query string, args []any, what string) diag.Diagnostics {
	var diags diag.Diagnostics

	var privileges pq.ByteaArray
	if err := txn.QueryRow(query, args...).Scan(&privileges); err != nil {
		diags.AddError("could not read privileges", fmt.Sprintf("could not read privileges for %s: %v", what, err))
		return diags
	}

	granted := pgArrayToSet(privileges)
	if !grantResourcePrivilegesEqual(granted, fields.privileges, fields.objectType, db) {
		pset, dd := sdkSetToFWSet(ctx, granted)
		diags.Append(dd...)
		if diags.HasError() {
			return diags
		}
		data.Privileges = pset
	}
	return diags
}

// readColumnRolePrivilegesFW reads per-column privileges for a column grant.
func (r *grantResource) readColumnRolePrivilegesFW(ctx context.Context, txn *sql.Tx, data *grantResourceModel, fields grantFields) diag.Diagnostics {
	var diags diag.Diagnostics

	objects := fields.objects
	columns := fields.columns

	// missingColumns starts as the configured columns; matched columns are
	// removed as the query returns them.
	missingColumns := sdkschema.NewSet(sdkschema.HashString, columns.List())

	query := `
SELECT relname AS table_name, attname AS column_name, array_agg(privilege_type) AS column_privileges
FROM (SELECT relname, attname, (aclexplode(attacl)).*
      FROM pg_class
               JOIN pg_namespace ON pg_class.relnamespace = pg_namespace.oid
               JOIN pg_attribute ON pg_class.oid = attrelid
      WHERE nspname = $2
        AND relname = $3
        AND relkind = $4)
         AS col_privs
         JOIN pg_roles ON pg_roles.oid = col_privs.grantee
WHERE rolname = $1
  AND privilege_type = $5
GROUP BY col_privs.relname, col_privs.attname, col_privs.privilege_type
ORDER BY col_privs.attname
;`
	rows, err := txn.Query(
		query, fields.role, fields.schemaName, objects.List()[0], objectTypes["table"], fields.privileges.List()[0],
	)
	if err != nil {
		diags.AddError("could not read column privileges", err.Error())
		return diags
	}
	defer rows.Close()

	for rows.Next() {
		var objName string
		var colName string
		var privileges pq.ByteaArray
		if err := rows.Scan(&objName, &colName, &privileges); err != nil {
			diags.AddError("could not scan column privileges", err.Error())
			return diags
		}

		if objects.Len() > 0 && !objects.Contains(objName) {
			continue
		}

		if missingColumns.Contains(colName) {
			missingColumns.Remove(colName)
		}

		privilegesSet := pgArrayToSet(privileges)
		if !privilegesSet.Equal(fields.privileges) {
			log.Printf(
				"[DEBUG] %s %s has not the expected privileges %v for role %s",
				strings.ToTitle("column"), objName, privileges, fields.role,
			)
			pset, dd := sdkSetToFWSet(ctx, privilegesSet)
			diags.Append(dd...)
			if diags.HasError() {
				return diags
			}
			data.Privileges = pset
			break
		}
	}

	if missingColumns.Len() > 0 {
		remainingColumns := columns.Difference(missingColumns)
		log.Printf("[DEBUG] Role %s does not have the expected privileges on columns", fields.role)
		cset, dd := sdkSetToFWSet(ctx, remainingColumns)
		diags.Append(dd...)
		if diags.HasError() {
			return diags
		}
		data.Columns = cset
	}

	return diags
}

// createGrantQueryFW builds the GRANT statement for the given fields and privileges.
func createGrantQueryFW(g grantFields, privileges []string) string {
	var query string

	switch strings.ToUpper(g.objectType) {
	case "DATABASE":
		query = fmt.Sprintf(
			"GRANT %s ON DATABASE %s TO %s",
			strings.Join(privileges, ","),
			pq.QuoteIdentifier(g.database),
			pq.QuoteIdentifier(g.role),
		)
	case "SCHEMA":
		query = fmt.Sprintf(
			"GRANT %s ON SCHEMA %s TO %s",
			strings.Join(privileges, ","),
			pq.QuoteIdentifier(g.schemaName),
			pq.QuoteIdentifier(g.role),
		)
	case "FOREIGN_DATA_WRAPPER":
		fdwName := g.objects.List()[0]
		query = fmt.Sprintf(
			"GRANT %s ON FOREIGN DATA WRAPPER %s TO %s",
			strings.Join(privileges, ","),
			pq.QuoteIdentifier(fdwName.(string)),
			pq.QuoteIdentifier(g.role),
		)
	case "FOREIGN_SERVER":
		srvName := g.objects.List()[0]
		query = fmt.Sprintf(
			"GRANT %s ON FOREIGN SERVER %s TO %s",
			strings.Join(privileges, ","),
			pq.QuoteIdentifier(srvName.(string)),
			pq.QuoteIdentifier(g.role),
		)
	case "COLUMN":
		query = fmt.Sprintf(
			"GRANT %s (%s) ON TABLE %s TO %s",
			strings.Join(privileges, ","),
			setToPgIdentListWithoutSchema(g.columns),
			setToPgIdentList(g.schemaName, g.objects),
			pq.QuoteIdentifier(g.role),
		)
	case "TABLE", "SEQUENCE", "FUNCTION", "PROCEDURE", "ROUTINE":
		if g.objects.Len() > 0 {
			query = fmt.Sprintf(
				"GRANT %s ON %s %s TO %s",
				strings.Join(privileges, ","),
				strings.ToUpper(g.objectType),
				setToPgIdentList(g.schemaName, g.objects),
				pq.QuoteIdentifier(g.role),
			)
		} else {
			query = fmt.Sprintf(
				"GRANT %s ON ALL %sS IN SCHEMA %s TO %s",
				strings.Join(privileges, ","),
				strings.ToUpper(g.objectType),
				pq.QuoteIdentifier(g.schemaName),
				pq.QuoteIdentifier(g.role),
			)
		}
	}

	if g.withGrantOption {
		query = query + " WITH GRANT OPTION"
	}

	return query
}

// createRevokeQueryFW builds the REVOKE statement for the given fields.
func createRevokeQueryFW(g grantFields) string {
	var query string

	switch strings.ToUpper(g.objectType) {
	case "DATABASE":
		query = fmt.Sprintf(
			"REVOKE ALL PRIVILEGES ON DATABASE %s FROM %s",
			pq.QuoteIdentifier(g.database),
			pq.QuoteIdentifier(g.role),
		)
	case "SCHEMA":
		query = fmt.Sprintf(
			"REVOKE ALL PRIVILEGES ON SCHEMA %s FROM %s",
			pq.QuoteIdentifier(g.schemaName),
			pq.QuoteIdentifier(g.role),
		)
	case "FOREIGN_DATA_WRAPPER":
		fdwName := g.objects.List()[0]
		query = fmt.Sprintf(
			"REVOKE ALL PRIVILEGES ON FOREIGN DATA WRAPPER %s FROM %s",
			pq.QuoteIdentifier(fdwName.(string)),
			pq.QuoteIdentifier(g.role),
		)
	case "FOREIGN_SERVER":
		srvName := g.objects.List()[0]
		query = fmt.Sprintf(
			"REVOKE ALL PRIVILEGES ON FOREIGN SERVER %s FROM %s",
			pq.QuoteIdentifier(srvName.(string)),
			pq.QuoteIdentifier(g.role),
		)
	case "COLUMN":
		if g.privileges.Len() == 0 || g.columns.Len() == 0 {
			// No privileges to revoke, so don't revoke anything
			query = "SELECT NULL"
		} else {
			query = fmt.Sprintf(
				"REVOKE %s (%s) ON TABLE %s FROM %s",
				setToPgIdentSimpleList(g.privileges),
				setToPgIdentListWithoutSchema(g.columns),
				setToPgIdentList(g.schemaName, g.objects),
				pq.QuoteIdentifier(g.role),
			)
		}
	case "TABLE", "SEQUENCE", "FUNCTION", "PROCEDURE", "ROUTINE":
		if g.objects.Len() > 0 {
			if g.privileges.Len() > 0 {
				// Revoking specific privileges instead of all privileges
				// to avoid messing with column level grants
				query = fmt.Sprintf(
					"REVOKE %s ON %s %s FROM %s",
					setToPgIdentSimpleList(g.privileges),
					strings.ToUpper(g.objectType),
					setToPgIdentList(g.schemaName, g.objects),
					pq.QuoteIdentifier(g.role),
				)
			} else {
				query = fmt.Sprintf(
					"REVOKE ALL PRIVILEGES ON %s %s FROM %s",
					strings.ToUpper(g.objectType),
					setToPgIdentList(g.schemaName, g.objects),
					pq.QuoteIdentifier(g.role),
				)
			}
		} else {
			query = fmt.Sprintf(
				"REVOKE ALL PRIVILEGES ON ALL %sS IN SCHEMA %s FROM %s",
				strings.ToUpper(g.objectType),
				pq.QuoteIdentifier(g.schemaName),
				pq.QuoteIdentifier(g.role),
			)
		}
	}

	return query
}

// grantRolePrivilegesFW executes the GRANT for the role's privileges.
func grantRolePrivilegesFW(txn *sql.Tx, g grantFields) error {
	privileges := []string{}
	for _, priv := range g.privileges.List() {
		privileges = append(privileges, priv.(string))
	}

	if len(privileges) == 0 {
		log.Printf("[DEBUG] no privileges to grant for role %s in database: %s,", g.role, g.database)
		return nil
	}

	query := createGrantQueryFW(g, privileges)

	_, err := txn.Exec(query)
	return err
}

// revokeRolePrivilegesFW executes the REVOKE for the role's privileges.
func revokeRolePrivilegesFW(txn *sql.Tx, g grantFields) error {
	query := createRevokeQueryFW(g)
	if len(query) == 0 {
		// Query is empty, don't run anything
		return nil
	}
	if _, err := txn.Exec(query); err != nil {
		return fmt.Errorf("could not execute revoke query: %w", err)
	}
	return nil
}

// generateGrantIDFW builds the synthetic grant id from role, database, schema,
// object_type, objects and columns.
func generateGrantIDFW(g grantFields) string {
	parts := []string{g.role, g.database}

	if g.objectType != "database" && g.objectType != "foreign_data_wrapper" && g.objectType != "foreign_server" {
		parts = append(parts, g.schemaName)
	}
	parts = append(parts, g.objectType)

	for _, object := range g.objects.List() {
		parts = append(parts, object.(string))
	}

	for _, column := range g.columns.List() {
		parts = append(parts, column.(string))
	}

	return strings.Join(parts, "_")
}

// getRolesToGrantFW returns the object/schema owners that must be temporarily
// granted to the connected user to apply the grant.
func getRolesToGrantFW(txn *sql.Tx, objectType, schemaName string) ([]string, error) {
	owners := []string{}

	if objectType == "database" || objectType == "foreign_data_wrapper" || objectType == "foreign_server" {
		return owners, nil
	}

	if objectType != "schema" {
		var err error
		owners, err = getTablesOwner(txn, schemaName)
		if err != nil {
			return nil, err
		}
	}

	schemaOwner, err := getSchemaOwner(txn, schemaName)
	if err != nil {
		return nil, err
	}
	if !sliceContainsStr(owners, schemaOwner) {
		owners = append(owners, schemaOwner)
	}

	owners, err = resolveOwners(txn, owners)
	if err != nil {
		return nil, err
	}

	return owners, nil
}

// checkRoleDBSchemaExistsFW reports whether the role, database and (when
// relevant) schema all exist.
func checkRoleDBSchemaExistsFW(db *DBConnection, role, database, pgSchema, objectType string) (bool, error) {
	exists, err := dbExists(db, database)
	if err != nil {
		return false, err
	}
	if !exists {
		log.Printf("[DEBUG] database %s does not exists", database)
		return false, nil
	}

	txn, err := startTransaction(db.client, database)
	if err != nil {
		return false, err
	}
	defer deferredRollback(txn)

	if role != publicRole {
		exists, err := roleExists(txn, role)
		if err != nil {
			return false, err
		}
		if !exists {
			log.Printf("[DEBUG] role %s does not exists", role)
			return false, nil
		}
	}

	if !sliceContainsStr([]string{"database", "foreign_data_wrapper", "foreign_server"}, objectType) && pgSchema != "" {
		exists, err = schemaExists(txn, pgSchema)
		if err != nil {
			return false, err
		}
		if !exists {
			log.Printf("[DEBUG] schema %s does not exists", pgSchema)
			return false, nil
		}
	}

	return true, nil
}

// validateFeatureSupportFW checks that the server supports privileges and the
// requested object type.
func validateFeatureSupportFW(db *DBConnection, objectType string) error {
	if !db.featureSupported(featurePrivileges) {
		return fmt.Errorf(
			"postgresql_grant resource is not supported for this Postgres version (%s)",
			db.version,
		)
	}
	if objectType == "procedure" && !db.featureSupported(featureProcedure) {
		return fmt.Errorf(
			"object type PROCEDURE is not supported for this Postgres version (%s)",
			db.version,
		)
	}
	if objectType == "routine" && !db.featureSupported(featureRoutine) {
		return fmt.Errorf(
			"object type ROUTINE is not supported for this Postgres version (%s)",
			db.version,
		)
	}
	return nil
}

// validateGrantPrivileges checks that every requested privilege is allowed for
// the object type.
func validateGrantPrivileges(db *DBConnection, objectType string, privileges *sdkschema.Set) error {
	allowed := allowedPrivilegesForObjectType(objectType, db)
	if allowed == nil {
		return fmt.Errorf("unknown object type %s", objectType)
	}
	for _, priv := range privileges.List() {
		if !sliceContainsStr(allowed, priv.(string)) {
			return fmt.Errorf("%s is not an allowed privilege for object type %s", priv, objectType)
		}
	}
	return nil
}

// grantResourcePrivilegesEqual reports whether the granted and wanted privilege
// sets are equivalent, expanding an implicit "ALL".
func grantResourcePrivilegesEqual(granted, wanted *sdkschema.Set, objectType string, db *DBConnection) bool {
	if granted.Equal(wanted) {
		return true
	}

	if !wanted.Contains("ALL") {
		return false
	}

	// implicit check: e.g. for object_type schema -> ALL == ["CREATE", "USAGE"]
	implicits := []any{}
	for _, p := range allowedPrivilegesForObjectType(objectType, db) {
		if p != "ALL" {
			implicits = append(implicits, p)
		}
	}
	wantedSet := sdkschema.NewSet(sdkschema.HashString, implicits)
	return granted.Equal(wantedSet)
}

// fwStringSetToSDK converts a framework string set into an *sdkschema.Set
// (HashString) so that identifier rendering and ordering are deterministic. A
// null/unknown set becomes an empty set.
func fwStringSetToSDK(ctx context.Context, s types.Set) (*sdkschema.Set, diag.Diagnostics) {
	var diags diag.Diagnostics
	var elems []string
	if !s.IsNull() && !s.IsUnknown() {
		diags = s.ElementsAs(ctx, &elems, false)
	}
	anys := make([]any, len(elems))
	for i, v := range elems {
		anys[i] = v
	}
	return sdkschema.NewSet(sdkschema.HashString, anys), diags
}

// sdkSetToFWSet converts an *sdkschema.Set of strings into a framework types.Set.
func sdkSetToFWSet(ctx context.Context, s *sdkschema.Set) (types.Set, diag.Diagnostics) {
	elems := make([]string, 0, s.Len())
	for _, v := range s.List() {
		elems = append(elems, v.(string))
	}
	return types.SetValueFrom(ctx, types.StringType, elems)
}
