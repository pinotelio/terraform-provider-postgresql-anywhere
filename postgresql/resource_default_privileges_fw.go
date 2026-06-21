package postgresql

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/lib/pq"
)

var (
	_ resource.Resource                = (*defaultPrivilegesResource)(nil)
	_ resource.ResourceWithConfigure   = (*defaultPrivilegesResource)(nil)
	_ resource.ResourceWithImportState = (*defaultPrivilegesResource)(nil)
)

// NewDefaultPrivilegesResource returns the postgresql_default_privileges resource.
func NewDefaultPrivilegesResource() resource.Resource {
	return &defaultPrivilegesResource{}
}

type defaultPrivilegesResource struct {
	client *Client
}

type defaultPrivilegesResourceModel struct {
	ID              types.String `tfsdk:"id"`
	Role            types.String `tfsdk:"role"`
	Database        types.String `tfsdk:"database"`
	Owner           types.String `tfsdk:"owner"`
	Schema          types.String `tfsdk:"schema"`
	ObjectType      types.String `tfsdk:"object_type"`
	Privileges      types.Set    `tfsdk:"privileges"`
	WithGrantOption types.Bool   `tfsdk:"with_grant_option"`
}

func (r *defaultPrivilegesResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_default_privileges"
}

func (r *defaultPrivilegesResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages default privileges for a PostgreSQL role.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Synthetic id: role_database_schema_owner_objecttype",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"role": schema.StringAttribute{
				Required:      true,
				Description:   "The name of the role to which grant default privileges on",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"database": schema.StringAttribute{
				Required:      true,
				Description:   "The database to grant default privileges for this role",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"owner": schema.StringAttribute{
				Required:      true,
				Description:   "Target role for which to alter default privileges.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"schema": schema.StringAttribute{
				Optional:      true,
				Description:   "The database schema to set default privileges for this role",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"object_type": schema.StringAttribute{
				Required:    true,
				Description: "The PostgreSQL object type to set the default privileges on (one of: table, sequence, function, routine, type, schema)",
				Validators: []validator.String{
					stringvalidator.OneOf("table", "sequence", "function", "routine", "type", "schema"),
				},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"privileges": schema.SetAttribute{
				ElementType: types.StringType,
				Required:    true,
				Description: "The list of privileges to apply as default privileges",
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

func (r *defaultPrivilegesResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *defaultPrivilegesResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data defaultPrivilegesResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.createOrUpdate(ctx, &data, &resp.Diagnostics, &resp.State)
}

func (r *defaultPrivilegesResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data defaultPrivilegesResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.createOrUpdate(ctx, &data, &resp.Diagnostics, &resp.State)
}

// createOrUpdate applies the configured default privileges and is wired as both
// Create and Update.
func (r *defaultPrivilegesResource) createOrUpdate(ctx context.Context, data *defaultPrivilegesResourceModel, diags *diag.Diagnostics, state *tfsdk.State) {
	db, err := r.client.Connect()
	if err != nil {
		diags.AddError("could not connect to database", err.Error())
		return
	}

	pgSchema := data.Schema.ValueString()
	objectType := data.ObjectType.ValueString()

	if pgSchema != "" && objectType == "schema" {
		if !db.featureSupported(featurePrivilegesOnSchemas) {
			diags.AddError(
				"feature not supported",
				fmt.Sprintf("changing default privileges for schemas is not supported for this Postgres version (%s)", db.version),
			)
			return
		}
		diags.AddError("invalid configuration", "cannot specify `schema` when `object_type` is `schema`")
		return
	}

	if objectType == "routine" && !db.featureSupported(featureRoutine) {
		diags.AddError(
			"feature not supported",
			fmt.Sprintf("object type ROUTINE is not supported for this Postgres version (%s)", db.version),
		)
		return
	}

	if data.WithGrantOption.ValueBool() && strings.ToLower(data.Role.ValueString()) == "public" {
		diags.AddError("invalid configuration", "with_grant_option cannot be true for role 'public'")
		return
	}

	privileges, d := defaultPrivilegesList(ctx, data.Privileges)
	diags.Append(d...)
	if diags.HasError() {
		return
	}

	if err := validateDefaultPrivileges(db, objectType, privileges); err != nil {
		diags.AddError("invalid privileges", err.Error())
		return
	}

	database := data.Database.ValueString()
	owner := data.Owner.ValueString()

	txn, err := startTransaction(r.client, database)
	if err != nil {
		diags.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	if err := pgLockRole(txn, owner); err != nil {
		diags.AddError("could not lock role", err.Error())
		return
	}

	// Needed in order to set the owner of the db if the connection user is not a superuser.
	if err := withRolesGranted(txn, []string{owner}, func() error {
		// Revoke all privileges before granting otherwise reducing privileges will not work.
		// We just have to revoke them in the same transaction so role will not lose its privileges
		// between revoke and grant.
		if err := revokeDefaultPrivileges(txn, data); err != nil {
			return err
		}
		return grantDefaultPrivileges(txn, data, privileges)
	}); err != nil {
		diags.AddError("could not alter default privileges", err.Error())
		return
	}

	if err := txn.Commit(); err != nil {
		diags.AddError("could not commit transaction", err.Error())
		return
	}

	data.ID = types.StringValue(defaultPrivilegesID(*data))

	// Read back to refresh the privileges set in state.
	txn2, err := startTransaction(r.client, database)
	if err != nil {
		diags.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn2)

	removed, d := r.readDefaultPrivileges(ctx, txn2, db, data)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	if removed {
		// Nothing got persisted; leave the resource absent.
		return
	}

	diags.Append(state.Set(ctx, data)...)
}

func (r *defaultPrivilegesResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data defaultPrivilegesResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	db, err := r.client.Connect()
	if err != nil {
		resp.Diagnostics.AddError("could not connect to database", err.Error())
		return
	}

	pgSchema := data.Schema.ValueString()
	objectType := data.ObjectType.ValueString()

	if pgSchema != "" && objectType == "schema" && !db.featureSupported(featurePrivilegesOnSchemas) {
		resp.Diagnostics.AddError(
			"feature not supported",
			fmt.Sprintf("changing default privileges for schemas is not supported for this Postgres version (%s)", db.version),
		)
		return
	}

	if objectType == "routine" && !db.featureSupported(featureRoutine) {
		resp.Diagnostics.AddError(
			"feature not supported",
			fmt.Sprintf("object type ROUTINE is not supported for this Postgres version (%s)", db.version),
		)
		return
	}

	exists, err := checkDefaultPrivilegesRoleDBSchemaExists(db, data)
	if err != nil {
		resp.Diagnostics.AddError("could not check prerequisites", err.Error())
		return
	}
	if !exists {
		resp.State.RemoveResource(ctx)
		return
	}

	txn, err := startTransaction(r.client, data.Database.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	removed, d := r.readDefaultPrivileges(ctx, txn, db, &data)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	if removed {
		resp.State.RemoveResource(ctx)
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *defaultPrivilegesResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data defaultPrivilegesResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	db, err := r.client.Connect()
	if err != nil {
		resp.Diagnostics.AddError("could not connect to database", err.Error())
		return
	}

	owner := data.Owner.ValueString()
	pgSchema := data.Schema.ValueString()
	objectType := data.ObjectType.ValueString()

	if pgSchema != "" && objectType == "schema" && !db.featureSupported(featurePrivilegesOnSchemas) {
		resp.Diagnostics.AddError(
			"feature not supported",
			fmt.Sprintf("changing default privileges for schemas is not supported for this Postgres version (%s)", db.version),
		)
		return
	}

	txn, err := startTransaction(r.client, data.Database.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	if err := pgLockRole(txn, owner); err != nil {
		resp.Diagnostics.AddError("could not lock role", err.Error())
		return
	}

	if err := withRolesGranted(txn, []string{owner}, func() error {
		return revokeDefaultPrivileges(txn, &data)
	}); err != nil {
		resp.Diagnostics.AddError("could not revoke default privileges", err.Error())
		return
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("could not commit transaction", err.Error())
		return
	}
}

// ImportState accepts the multi-part id "role_database_schema_owner_objecttype".
func (r *defaultPrivilegesResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(req.ID, "_")
	if len(parts) != 5 {
		resp.Diagnostics.AddError(
			"invalid import ID",
			"expected format \"role_database_schema_owner_objecttype\" (use \"noschema\" for the schema part when none was set)",
		)
		return
	}

	pgSchema := parts[2]
	if pgSchema == "noschema" {
		pgSchema = ""
	}

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("role"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("database"), parts[1])...)
	if pgSchema == "" {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("schema"), types.StringNull())...)
	} else {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("schema"), pgSchema)...)
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("owner"), parts[3])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("object_type"), parts[4])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

// readDefaultPrivileges loads the current default privileges into the model. It
// updates data.Privileges/data.ID and returns true when the resource should be
// treated as no longer existing.
func (r *defaultPrivilegesResource) readDefaultPrivileges(ctx context.Context, txn *sql.Tx, db *DBConnection, data *defaultPrivilegesResourceModel) (bool, diag.Diagnostics) {
	var diags diag.Diagnostics

	role := data.Role.ValueString()
	owner := data.Owner.ValueString()
	pgSchema := data.Schema.ValueString()
	objectType := data.ObjectType.ValueString()

	privilegesInput, d := defaultPrivilegesList(ctx, data.Privileges)
	diags.Append(d...)
	if diags.HasError() {
		return false, diags
	}

	if err := pgLockRole(txn, owner); err != nil {
		diags.AddError("could not lock role", err.Error())
		return false, diags
	}

	roleOID, err := getRoleOID(txn, role)
	if err != nil {
		diags.AddError("could not get role OID", err.Error())
		return false, diags
	}

	var query string
	var queryArgs []any

	// This query aggregates the list of default privileges type (prtype)
	// for the role (grantee), owner (grantor), schema (namespace name)
	// and the specified object type (defaclobjtype).
	if pgSchema != "" {
		query = `SELECT array_agg(prtype) FROM (
		SELECT defaclnamespace, (aclexplode(defaclacl)).* FROM pg_default_acl
		WHERE defaclobjtype = $3
	) AS t (namespace, grantor_oid, grantee_oid, prtype, grantable)
	JOIN pg_namespace ON pg_namespace.oid = namespace
	WHERE grantee_oid = $1 AND nspname = $2 AND pg_get_userbyid(grantor_oid) = $4;
`
		queryArgs = []any{roleOID, pgSchema, objectTypes[objectType], owner}
	} else {
		query = `SELECT array_agg(prtype) FROM (
		SELECT defaclnamespace, (aclexplode(defaclacl)).* FROM pg_default_acl
		WHERE defaclobjtype = $2
	) AS t (namespace, grantor_oid, grantee_oid, prtype, grantable)
	WHERE grantee_oid = $1 AND namespace = 0 AND pg_get_userbyid(grantor_oid) = $3;
`
		queryArgs = []any{roleOID, objectTypes[objectType], owner}
	}

	var privileges pq.ByteaArray
	if err := txn.QueryRow(query, queryArgs...).Scan(&privileges); err != nil {
		diags.AddError("could not read default privileges", err.Error())
		return false, diags
	}

	// We consider no privileges as "not exists" unless no privileges were provided as input.
	if len(privileges) == 0 {
		log.Printf("[DEBUG] no default privileges for role %s in schema %s", role, pgSchema)
		if len(privilegesInput) != 0 {
			return true, diags
		}
	}

	grantedPrivileges := byteaArrayToStringSlice(privileges)

	if !defaultPrivilegesEqual(grantedPrivileges, privilegesInput, db, objectType) {
		pset, dd := types.SetValueFrom(ctx, types.StringType, grantedPrivileges)
		diags.Append(dd...)
		if diags.HasError() {
			return false, diags
		}
		data.Privileges = pset
	}

	data.ID = types.StringValue(defaultPrivilegesID(*data))

	return false, diags
}

// grantDefaultPrivileges grants the given default privileges in the transaction.
func grantDefaultPrivileges(txn *sql.Tx, data *defaultPrivilegesResourceModel, privileges []string) error {
	role := data.Role.ValueString()
	pgSchema := data.Schema.ValueString()

	if len(privileges) == 0 {
		log.Printf("[DEBUG] no default privileges to grant for role %s, owner %s in database: %s,", data.Role.ValueString(), data.Owner.ValueString(), data.Database.ValueString())
		return nil
	}

	var inSchema string
	// If a schema is specified we need to build the part of the query string to action this.
	if pgSchema != "" {
		inSchema = fmt.Sprintf("IN SCHEMA %s", pq.QuoteIdentifier(pgSchema))
	}

	query := fmt.Sprintf("ALTER DEFAULT PRIVILEGES FOR ROLE %s %s GRANT %s ON %sS TO %s",
		pq.QuoteIdentifier(data.Owner.ValueString()),
		inSchema,
		strings.Join(privileges, ","),
		strings.ToUpper(data.ObjectType.ValueString()),
		pq.QuoteIdentifier(role),
	)

	if data.WithGrantOption.ValueBool() {
		query = query + " WITH GRANT OPTION"
	}

	if _, err := txn.Exec(query); err != nil {
		return fmt.Errorf("could not alter default privileges: %w", err)
	}

	return nil
}

// revokeDefaultPrivileges revokes all default privileges in the transaction.
func revokeDefaultPrivileges(txn *sql.Tx, data *defaultPrivilegesResourceModel) error {
	pgSchema := data.Schema.ValueString()

	var inSchema string
	// If a schema is specified we need to build the part of the query string to action this.
	if pgSchema != "" {
		inSchema = fmt.Sprintf("IN SCHEMA %s", pq.QuoteIdentifier(pgSchema))
	}

	query := fmt.Sprintf(
		"ALTER DEFAULT PRIVILEGES FOR ROLE %s %s REVOKE ALL ON %sS FROM %s",
		pq.QuoteIdentifier(data.Owner.ValueString()),
		inSchema,
		strings.ToUpper(data.ObjectType.ValueString()),
		pq.QuoteIdentifier(data.Role.ValueString()),
	)

	if _, err := txn.Exec(query); err != nil {
		return fmt.Errorf("could not revoke default privileges: %w", err)
	}
	return nil
}

// defaultPrivilegesID builds the synthetic id
// "role_database_schema_owner_objecttype".
func defaultPrivilegesID(m defaultPrivilegesResourceModel) string {
	pgSchema := m.Schema.ValueString()
	if pgSchema == "" {
		pgSchema = "noschema"
	}

	return strings.Join([]string{
		m.Role.ValueString(), m.Database.ValueString(), pgSchema,
		m.Owner.ValueString(), m.ObjectType.ValueString(),
	}, "_")
}

// defaultPrivilegesList extracts the privileges set into a []string.
func defaultPrivilegesList(ctx context.Context, set types.Set) ([]string, diag.Diagnostics) {
	var privileges []string
	if set.IsNull() || set.IsUnknown() {
		return privileges, nil
	}
	d := set.ElementsAs(ctx, &privileges, false)
	return privileges, d
}

func byteaArrayToStringSlice(arr pq.ByteaArray) []string {
	s := make([]string, len(arr))
	for i, v := range arr {
		s[i] = string(v)
	}
	return s
}

// validateDefaultPrivileges checks that every privilege is allowed for the object type.
func validateDefaultPrivileges(db *DBConnection, objectType string, privileges []string) error {
	allowed := allowedPrivilegesForObjectType(objectType, db)
	if allowed == nil {
		return fmt.Errorf("unknown object type %s", objectType)
	}
	for _, priv := range privileges {
		if !sliceContainsStr(allowed, priv) {
			return fmt.Errorf("%s is not an allowed privilege for object type %s", priv, objectType)
		}
	}
	return nil
}

// defaultPrivilegesEqual compares the granted privileges (read from the DB)
// against the wanted privileges (input), including the implicit "ALL" expansion.
func defaultPrivilegesEqual(granted, wanted []string, db *DBConnection, objectType string) bool {
	if stringSetsEqual(granted, wanted) {
		return true
	}

	wantedHasAll := false
	for _, w := range wanted {
		if w == "ALL" {
			wantedHasAll = true
			break
		}
	}
	if !wantedHasAll {
		return false
	}

	// implicit check: e.g. for object_type schema -> ALL == ["CREATE", "USAGE"]
	implicits := []string{}
	for _, p := range allowedPrivilegesForObjectType(objectType, db) {
		if p != "ALL" {
			implicits = append(implicits, p)
		}
	}
	return stringSetsEqual(granted, implicits)
}

func stringSetsEqual(a, b []string) bool {
	am := make(map[string]struct{}, len(a))
	for _, v := range a {
		am[v] = struct{}{}
	}
	bm := make(map[string]struct{}, len(b))
	for _, v := range b {
		bm[v] = struct{}{}
	}
	if len(am) != len(bm) {
		return false
	}
	for k := range am {
		if _, ok := bm[k]; !ok {
			return false
		}
	}
	return true
}

// checkDefaultPrivilegesRoleDBSchemaExists reports whether the database, role,
// and schema referenced by the resource all exist.
func checkDefaultPrivilegesRoleDBSchemaExists(db *DBConnection, data defaultPrivilegesResourceModel) (bool, error) {
	database := data.Database.ValueString()

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

	role := data.Role.ValueString()
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

	pgSchema := data.Schema.ValueString()
	if pgSchema != "" {
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
