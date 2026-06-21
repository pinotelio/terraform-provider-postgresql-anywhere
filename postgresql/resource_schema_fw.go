package postgresql

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/lib/pq"
	acl "github.com/sean-/postgresql-acl"
)

var (
	_ resource.Resource                = (*schemaResource)(nil)
	_ resource.ResourceWithConfigure   = (*schemaResource)(nil)
	_ resource.ResourceWithImportState = (*schemaResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*schemaResource)(nil)
)

// NewSchemaResource returns the postgresql_schema resource.
func NewSchemaResource() resource.Resource {
	return &schemaResource{}
}

type schemaResource struct {
	client *Client
}

type schemaResourceModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Database    types.String `tfsdk:"database"`
	Owner       types.String `tfsdk:"owner"`
	IfNotExists types.Bool   `tfsdk:"if_not_exists"`
	DropCascade types.Bool   `tfsdk:"drop_cascade"`
	Policy      types.Set    `tfsdk:"policy"`
}

type schemaPolicyModel struct {
	Create          types.Bool   `tfsdk:"create"`
	CreateWithGrant types.Bool   `tfsdk:"create_with_grant"`
	Role            types.String `tfsdk:"role"`
	Usage           types.Bool   `tfsdk:"usage"`
	UsageWithGrant  types.Bool   `tfsdk:"usage_with_grant"`
}

func (r *schemaResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_schema"
}

func (r *schemaResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a PostgreSQL schema.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Synthetic id: database.name",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "The name of the schema",
			},
			"database": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "The database name to alter schema",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"owner": schema.StringAttribute{
				Optional:      true,
				Computed:      true,
				Description:   "The ROLE name who owns the schema",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"if_not_exists": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(true),
				Description: "When true, use the existing schema if it exists",
			},
			"drop_cascade": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "When true, will also drop all the objects that are contained in the schema",
			},
		},
		Blocks: map[string]schema.Block{
			"policy": schema.SetNestedBlock{
				DeprecationMessage: "Use postgresql_grant resource instead (with object_type=\"schema\")",
				NestedObject: schema.NestedBlockObject{
					// These attributes are intentionally NOT Computed (and therefore
					// carry no Default). In a SetNestedBlock, marking nested
					// attributes Computed leaves unmatched/new elements as
					// wholly-unknown objects, which the framework rejects as
					// "Duplicate Set Element" whenever a schema declares more than
					// one policy block. Omitted booleans/strings read back as
					// false/"" via ValueBool()/ValueString(), giving the default
					// false / PUBLIC behavior.
					Attributes: map[string]schema.Attribute{
						"create": schema.BoolAttribute{
							Optional:    true,
							Description: "If true, allow the specified ROLEs to CREATE new objects within the schema(s)",
						},
						"create_with_grant": schema.BoolAttribute{
							Optional:    true,
							Description: "If true, allow the specified ROLEs to CREATE new objects within the schema(s) and GRANT the same CREATE privilege to different ROLEs",
						},
						"role": schema.StringAttribute{
							Optional:    true,
							Description: "ROLE who will receive this policy (default: PUBLIC)",
						},
						"usage": schema.BoolAttribute{
							Optional:    true,
							Description: "If true, allow the specified ROLEs to use objects within the schema(s)",
						},
						"usage_with_grant": schema.BoolAttribute{
							Optional:    true,
							Description: "If true, allow the specified ROLEs to use objects within the schema(s) and GRANT the same USAGE privilege to different ROLEs",
						},
					},
				},
			},
		},
	}
}

func (r *schemaResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// ModifyPlan keeps the computed id in sync with database.name. name is updatable
// in place (ALTER SCHEMA ... RENAME) and the id equals database.name, so on a
// rename the planned id must change too — otherwise the id plan modifier
// (UseStateForUnknown) would keep the old value while apply writes the new one,
// yielding a "provider produced inconsistent result" error.
func (r *schemaResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		// Resource is being destroyed.
		return
	}
	var name, database types.String
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("name"), &name)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("database"), &database)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if name.IsUnknown() || name.IsNull() || database.IsUnknown() || database.IsNull() {
		return
	}
	id := types.StringValue(fmt.Sprintf("%s.%s", database.ValueString(), name.ValueString()))
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("id"), id)...)
}

func (r *schemaResource) resolveDatabase(m schemaResourceModel) string {
	if m.Database.IsNull() || m.Database.IsUnknown() || m.Database.ValueString() == "" {
		return r.client.DatabaseName()
	}
	return m.Database.ValueString()
}

// schemaPolicyModelToACL converts a policy block into the ACL representation used
// to render GRANT/REVOKE statements.
func schemaPolicyModelToACL(p schemaPolicyModel) acl.Schema {
	var rolePolicy acl.Schema

	if p.Create.ValueBool() {
		rolePolicy.Privileges |= acl.Create
	}
	if p.CreateWithGrant.ValueBool() {
		rolePolicy.Privileges |= acl.Create
		rolePolicy.GrantOptions |= acl.Create
	}
	if p.Usage.ValueBool() {
		rolePolicy.Privileges |= acl.Usage
	}
	if p.UsageWithGrant.ValueBool() {
		rolePolicy.Privileges |= acl.Usage
		rolePolicy.GrantOptions |= acl.Usage
	}
	rolePolicy.Role = p.Role.ValueString()

	return rolePolicy
}

func (r *schemaResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data schemaResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	database := r.resolveDatabase(data)
	schemaName := data.Name.ValueString()

	db, err := r.client.ForDatabase(database).Connect()
	if err != nil {
		resp.Diagnostics.AddError("could not connect to database", err.Error())
		return
	}

	var policies []schemaPolicyModel
	if !data.Policy.IsNull() && !data.Policy.IsUnknown() {
		resp.Diagnostics.Append(data.Policy.ElementsAs(ctx, &policies, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	txn, err := startTransaction(r.client, database)
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	// If the authenticated user is not a superuser (e.g. on AWS RDS)
	// we'll need to temporarily grant it membership in the following roles:
	//  * the owner of the db (to have the permissions to create the schema)
	//  * the owner of the schema, if it has one (in order to change its owner)
	var rolesToGrant []string

	dbOwner, err := getDatabaseOwner(txn, database)
	if err != nil {
		resp.Diagnostics.AddError("could not get database owner", err.Error())
		return
	}
	rolesToGrant = append(rolesToGrant, dbOwner)

	schemaOwner := data.Owner.ValueString()
	if schemaOwner != "" && schemaOwner != dbOwner {
		rolesToGrant = append(rolesToGrant, schemaOwner)
	}

	if err := withRolesGranted(txn, rolesToGrant, func() error {
		return createSchemaFW(db, txn, schemaName, schemaOwner, data.IfNotExists.ValueBool(), policies)
	}); err != nil {
		resp.Diagnostics.AddError("could not create schema", err.Error())
		return
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("error committing schema", err.Error())
		return
	}

	owner, exists, err := r.readSchema(database, schemaName)
	if err != nil {
		resp.Diagnostics.AddError("error reading schema", err.Error())
		return
	}
	if !exists {
		resp.Diagnostics.AddError("could not create schema", "schema not found after creation")
		return
	}

	data.Name = types.StringValue(schemaName)
	data.Owner = types.StringValue(owner)
	data.Database = types.StringValue(database)
	data.ID = types.StringValue(fmt.Sprintf("%s.%s", database, schemaName))
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// createSchemaFW creates the schema (or sets the owner if it already exists) and
// applies the policy GRANTs, all within txn.
func createSchemaFW(db *DBConnection, txn *sql.Tx, schemaName, schemaOwner string, ifNotExists bool, policies []schemaPolicyModel) error {
	// Check whether the schema already exists.
	var foundSchema bool
	err := txn.QueryRow(`SELECT TRUE FROM pg_catalog.pg_namespace WHERE nspname = $1`, schemaName).Scan(&foundSchema)

	queries := []string{}
	switch {
	case err == sql.ErrNoRows:
		b := bytes.NewBufferString("CREATE SCHEMA ")
		if db.featureSupported(featureSchemaCreateIfNotExist) {
			if ifNotExists {
				fmt.Fprint(b, "IF NOT EXISTS ")
			}
		}
		fmt.Fprint(b, pq.QuoteIdentifier(schemaName))

		if schemaOwner != "" {
			fmt.Fprint(b, " AUTHORIZATION ", pq.QuoteIdentifier(schemaOwner))
		}
		queries = append(queries, b.String())

	case err != nil:
		return fmt.Errorf("error looking for schema: %w", err)

	default:
		// The schema already exists, we just set the owner.
		if schemaOwner != "" {
			if err := schemaSetOwnerFW(txn, schemaName, schemaOwner); err != nil {
				return err
			}
		}
	}

	// ACL objects that can generate the necessary SQL.
	type RoleKey string
	schemaPolicies := make(map[RoleKey]acl.Schema, len(policies))

	for _, p := range policies {
		rolePolicy := schemaPolicyModelToACL(p)

		roleKey := RoleKey(strings.ToLower(rolePolicy.Role))
		if existingRolePolicy, ok := schemaPolicies[roleKey]; ok {
			schemaPolicies[roleKey] = existingRolePolicy.Merge(rolePolicy)
		} else {
			schemaPolicies[roleKey] = rolePolicy
		}
	}

	for _, policy := range schemaPolicies {
		queries = append(queries, policy.Grants(schemaName)...)
	}

	for _, query := range queries {
		if _, err = txn.Exec(query); err != nil {
			return fmt.Errorf("error creating schema %s: %w", schemaName, err)
		}
	}

	return nil
}

// schemaSetOwnerFW issues ALTER SCHEMA ... OWNER TO ... within the given txn.
func schemaSetOwnerFW(txn *sql.Tx, schemaName, schemaOwner string) error {
	if schemaOwner == "" {
		return errors.New("error setting schema owner to an empty string")
	}

	sql := fmt.Sprintf("ALTER SCHEMA %s OWNER TO %s", pq.QuoteIdentifier(schemaName), pq.QuoteIdentifier(schemaOwner))
	if _, err := txn.Exec(sql); err != nil {
		return fmt.Errorf("error updating schema OWNER: %w", err)
	}

	return nil
}

// readSchema reads back a schema's owner. It returns whether the schema exists
// and validates the ACL items.
func (r *schemaResource) readSchema(database, schemaName string) (string, bool, error) {
	txn, err := startTransaction(r.client, database)
	if err != nil {
		return "", false, err
	}
	defer deferredRollback(txn)

	var schemaOwner string
	var schemaACLs []string
	err = txn.QueryRow("SELECT pg_catalog.pg_get_userbyid(n.nspowner), COALESCE(n.nspacl, '{}'::aclitem[])::TEXT[] FROM pg_catalog.pg_namespace n WHERE n.nspname=$1", schemaName).Scan(&schemaOwner, pq.Array(&schemaACLs))
	switch {
	case err == sql.ErrNoRows:
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("error reading schema: %w", err)
	}

	for _, aclStr := range schemaACLs {
		aclItem, err := acl.Parse(aclStr)
		if err != nil {
			return "", false, fmt.Errorf("error parsing aclitem: %w", err)
		}
		if _, err := acl.NewSchema(aclItem); err != nil {
			return "", false, fmt.Errorf("invalid perms for schema: %w", err)
		}
	}

	return schemaOwner, true, nil
}

func (r *schemaResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data schemaResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	database := r.resolveDatabase(data)
	schemaName := data.Name.ValueString()

	owner, exists, err := r.readSchema(database, schemaName)
	if err != nil {
		resp.Diagnostics.AddError("error reading schema", err.Error())
		return
	}
	if !exists {
		resp.State.RemoveResource(ctx)
		return
	}

	data.Name = types.StringValue(schemaName)
	data.Owner = types.StringValue(owner)
	data.Database = types.StringValue(database)
	data.ID = types.StringValue(fmt.Sprintf("%s.%s", database, schemaName))
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *schemaResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state schemaResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	databaseName := r.resolveDatabase(plan)

	txn, err := startTransaction(r.client, databaseName)
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	// Rename the schema if its name changed.
	if plan.Name.ValueString() != state.Name.ValueString() {
		if plan.Name.ValueString() == "" {
			resp.Diagnostics.AddError("invalid schema name", "error setting schema name to an empty string")
			return
		}
		stmt := fmt.Sprintf("ALTER SCHEMA %s RENAME TO %s",
			pq.QuoteIdentifier(state.Name.ValueString()),
			pq.QuoteIdentifier(plan.Name.ValueString()),
		)
		if _, err := txn.Exec(stmt); err != nil {
			resp.Diagnostics.AddError("error updating schema NAME", err.Error())
			return
		}
	}

	schemaName := plan.Name.ValueString()

	// Update the owner if it changed.
	if plan.Owner.ValueString() != state.Owner.ValueString() {
		if err := schemaSetOwnerFW(txn, schemaName, plan.Owner.ValueString()); err != nil {
			resp.Diagnostics.AddError("error updating schema owner", err.Error())
			return
		}
	}

	// Update the policy if it changed.
	if err := r.setSchemaPolicy(ctx, txn, plan, state); err != nil {
		resp.Diagnostics.AddError("error updating schema policy", err.Error())
		return
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("error committing schema", err.Error())
		return
	}

	owner, exists, err := r.readSchema(databaseName, schemaName)
	if err != nil {
		resp.Diagnostics.AddError("error reading schema", err.Error())
		return
	}
	if !exists {
		resp.State.RemoveResource(ctx)
		return
	}

	plan.Name = types.StringValue(schemaName)
	plan.Owner = types.StringValue(owner)
	plan.Database = types.StringValue(databaseName)
	plan.ID = types.StringValue(fmt.Sprintf("%s.%s", databaseName, schemaName))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *schemaResource) setSchemaPolicy(ctx context.Context, txn *sql.Tx, plan, state schemaResourceModel) error {
	schemaName := plan.Name.ValueString()
	owner := plan.Owner.ValueString()

	var oldList, newList []schemaPolicyModel
	if !state.Policy.IsNull() && !state.Policy.IsUnknown() {
		if diags := state.Policy.ElementsAs(ctx, &oldList, false); diags.HasError() {
			return fmt.Errorf("could not read previous schema policy")
		}
	}
	if !plan.Policy.IsNull() && !plan.Policy.IsUnknown() {
		if diags := plan.Policy.ElementsAs(ctx, &newList, false); diags.HasError() {
			return fmt.Errorf("could not read schema policy")
		}
	}

	dropped, added, updated := schemaChangedPoliciesFW(oldList, newList)
	if len(dropped) == 0 && len(added) == 0 && len(updated) == 0 {
		return nil
	}

	queries := make([]string, 0, len(oldList)+len(newList))

	for _, p := range dropped {
		rolePolicy := schemaPolicyModelToACL(p)

		// The PUBLIC role can not be DROP'ed, therefore we do not need
		// to prevent revoking against it not existing.
		if rolePolicy.Role != "" {
			var foundUser bool
			err := txn.QueryRow(`SELECT TRUE FROM pg_catalog.pg_roles WHERE rolname = $1`, rolePolicy.Role).Scan(&foundUser)
			switch {
			case err == sql.ErrNoRows:
				// Don't execute this role's REVOKEs because the role
				// was dropped first and therefore doesn't exist.
			case err != nil:
				return fmt.Errorf("error reading schema: %w", err)
			default:
				queries = append(queries, rolePolicy.Revokes(schemaName)...)
			}
		}
	}

	for _, p := range added {
		rolePolicy := schemaPolicyModelToACL(p)
		queries = append(queries, rolePolicy.Grants(schemaName)...)
	}

	for _, pair := range updated {
		oldPolicy := schemaPolicyModelToACL(pair[0])
		queries = append(queries, oldPolicy.Revokes(schemaName)...)

		newPolicy := schemaPolicyModelToACL(pair[1])
		queries = append(queries, newPolicy.Grants(schemaName)...)
	}

	rolesToGrant := []string{}
	if owner != "" {
		rolesToGrant = append(rolesToGrant, owner)
	}

	return withRolesGranted(txn, rolesToGrant, func() error {
		for _, query := range queries {
			if _, err := txn.Exec(query); err != nil {
				return fmt.Errorf("error updating schema DCL: %w", err)
			}
		}
		return nil
	})
}

// schemaChangedPoliciesFW walks old and new policy lists keyed by (lower-cased)
// role to determine which policies were dropped, added, or updated.
func schemaChangedPoliciesFW(old, new []schemaPolicyModel) (dropped, added []schemaPolicyModel, updated [][2]schemaPolicyModel) {
	oldLookupMap := make(map[string]schemaPolicyModel, len(old))
	for _, p := range old {
		oldLookupMap[strings.ToLower(p.Role.ValueString())] = p
	}

	newLookupMap := make(map[string]schemaPolicyModel, len(new))
	for _, p := range new {
		newLookupMap[strings.ToLower(p.Role.ValueString())] = p
	}

	for kOld, vOld := range oldLookupMap {
		if _, ok := newLookupMap[kOld]; !ok {
			dropped = append(dropped, vOld)
		}
	}

	for kNew, vNew := range newLookupMap {
		if _, ok := oldLookupMap[kNew]; !ok {
			added = append(added, vNew)
		}
	}

	for kOld, vOld := range oldLookupMap {
		if vNew, ok := newLookupMap[kOld]; ok {
			if !schemaPolicyModelEqual(vOld, vNew) {
				updated = append(updated, [2]schemaPolicyModel{vOld, vNew})
			}
		}
	}

	return dropped, added, updated
}

func schemaPolicyModelEqual(a, b schemaPolicyModel) bool {
	return a.Create.ValueBool() == b.Create.ValueBool() &&
		a.CreateWithGrant.ValueBool() == b.CreateWithGrant.ValueBool() &&
		a.Usage.ValueBool() == b.Usage.ValueBool() &&
		a.UsageWithGrant.ValueBool() == b.UsageWithGrant.ValueBool() &&
		a.Role.ValueString() == b.Role.ValueString()
}

func (r *schemaResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data schemaResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	database := r.resolveDatabase(data)
	schemaName := data.Name.ValueString()

	txn, err := startTransaction(r.client, database)
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	exists, err := schemaExists(txn, schemaName)
	if err != nil {
		resp.Diagnostics.AddError("error checking schema existence", err.Error())
		return
	}
	if !exists {
		return
	}

	owner := data.Owner.ValueString()

	if err := withRolesGranted(txn, []string{owner}, func() error {
		dropMode := "RESTRICT"
		if data.DropCascade.ValueBool() {
			dropMode = "CASCADE"
		}

		sql := fmt.Sprintf("DROP SCHEMA %s %s", pq.QuoteIdentifier(schemaName), dropMode)
		if _, err := txn.Exec(sql); err != nil {
			return fmt.Errorf("error deleting schema: %w", err)
		}

		return nil
	}); err != nil {
		resp.Diagnostics.AddError("could not drop schema", err.Error())
		return
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("error committing schema", err.Error())
	}
}

// ImportState accepts "database.name" (which is also the id).
func (r *schemaResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parsed := strings.Split(req.ID, ".")
	if len(parsed) != 2 {
		resp.Diagnostics.AddError(
			"invalid import ID",
			fmt.Sprintf("schema ID %s has not the expected format 'database.schema': %v", req.ID, parsed),
		)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("database"), parsed[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), parsed[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
