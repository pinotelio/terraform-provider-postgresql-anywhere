package postgresql

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/lib/pq"
)

var (
	_ resource.Resource                = (*grantRoleResource)(nil)
	_ resource.ResourceWithConfigure   = (*grantRoleResource)(nil)
	_ resource.ResourceWithImportState = (*grantRoleResource)(nil)
)

const (
	// This returns the role membership for role, grant_role
	getGrantRoleQuery = `
SELECT
  pg_get_userbyid(member) as role,
  pg_get_userbyid(roleid) as grant_role,
  admin_option
FROM
  pg_auth_members
WHERE
  pg_get_userbyid(member) = $1 AND
  pg_get_userbyid(roleid) = $2;
`
)

// NewGrantRoleResource returns the postgresql_grant_role resource.
func NewGrantRoleResource() resource.Resource {
	return &grantRoleResource{}
}

type grantRoleResource struct {
	client *Client
}

type grantRoleResourceModel struct {
	ID              types.String `tfsdk:"id"`
	Role            types.String `tfsdk:"role"`
	GrantRole       types.String `tfsdk:"grant_role"`
	WithAdminOption types.Bool   `tfsdk:"with_admin_option"`
}

func (r *grantRoleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_grant_role"
}

func (r *grantRoleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages the membership of a role in another role.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Synthetic id: role_grant_role_with_admin_option",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"role": schema.StringAttribute{
				Required:      true,
				Description:   "The name of the role to grant grant_role",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"grant_role": schema.StringAttribute{
				Required:      true,
				Description:   "The name of the role that is granted to role",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"with_admin_option": schema.BoolAttribute{
				Optional:      true,
				Computed:      true,
				Default:       booldefault.StaticBool(false),
				Description:   "Permit the grant recipient to grant it to others",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},
		},
	}
}

func (r *grantRoleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// connect opens a DB connection and verifies the privileges feature is
// supported by the connected PostgreSQL version.
func (r *grantRoleResource) connect(diags *diag.Diagnostics) *DBConnection {
	db, err := r.client.Connect()
	if err != nil {
		diags.AddError("could not connect to database", err.Error())
		return nil
	}
	if !db.featureSupported(featurePrivileges) {
		diags.AddError(
			"feature not supported",
			fmt.Sprintf("postgresql_grant_role resource is not supported for this Postgres version (%s)", db.version),
		)
		return nil
	}
	return db
}

func (r *grantRoleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data grantRoleResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	db := r.connect(&resp.Diagnostics)
	if db == nil {
		return
	}

	role := data.Role.ValueString()
	grantRoleName := data.GrantRole.ValueString()
	withAdmin := data.WithAdminOption.ValueBool()

	txn, err := startTransaction(r.client, "")
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	// Revoke the granted role before granting it again.
	if _, err := txn.Exec(grantRoleRevokeQuery(role, grantRoleName)); err != nil {
		resp.Diagnostics.AddError("could not execute revoke query", err.Error())
		return
	}

	if _, err := txn.Exec(grantRoleGrantQuery(role, grantRoleName, withAdmin)); err != nil {
		resp.Diagnostics.AddError("could not execute grant query", err.Error())
		return
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("could not commit transaction", err.Error())
		return
	}

	// Read back the freshly created membership to populate computed values.
	rName, grName, dbAdmin, found, err := grantRoleReadMembership(db, role, grantRoleName)
	if err != nil {
		resp.Diagnostics.AddError("error reading grant role", err.Error())
		return
	}
	if found {
		data.Role = types.StringValue(rName)
		data.GrantRole = types.StringValue(grName)
		data.WithAdminOption = types.BoolValue(dbAdmin)
	}

	data.ID = types.StringValue(grantRoleGenerateID(
		data.Role.ValueString(),
		data.GrantRole.ValueString(),
		data.WithAdminOption.ValueBool(),
	))
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *grantRoleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data grantRoleResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	db := r.connect(&resp.Diagnostics)
	if db == nil {
		return
	}

	rName, grName, dbAdmin, found, err := grantRoleReadMembership(db, data.Role.ValueString(), data.GrantRole.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("error reading grant role", err.Error())
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}

	data.Role = types.StringValue(rName)
	data.GrantRole = types.StringValue(grName)
	data.WithAdminOption = types.BoolValue(dbAdmin)
	data.ID = types.StringValue(grantRoleGenerateID(rName, grName, dbAdmin))
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// Update never makes changes (all attributes force replacement); it persists plan.
func (r *grantRoleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data grantRoleResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *grantRoleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data grantRoleResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	db := r.connect(&resp.Diagnostics)
	if db == nil {
		return
	}

	txn, err := startTransaction(r.client, "")
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	if _, err := txn.Exec(grantRoleRevokeQuery(data.Role.ValueString(), data.GrantRole.ValueString())); err != nil {
		resp.Diagnostics.AddError("could not execute revoke query", err.Error())
		return
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("could not commit transaction", err.Error())
	}
}

// ImportState accepts the synthetic id "role_grant_role_with_admin_option".
// The parser requires exactly three "_"-separated segments, so roles whose
// names contain underscores cannot be imported.
func (r *grantRoleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(req.ID, "_")
	if len(parts) != 3 {
		resp.Diagnostics.AddError(
			"invalid import ID",
			"expected format \"role_grant_role_with_admin_option\" (roles containing underscores cannot be imported)",
		)
		return
	}
	withAdmin, err := strconv.ParseBool(parts[2])
	if err != nil {
		resp.Diagnostics.AddError(
			"invalid import ID",
			fmt.Sprintf("could not parse with_admin_option %q as a boolean: %s", parts[2], err.Error()),
		)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("role"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("grant_role"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("with_admin_option"), withAdmin)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

// grantRoleReadMembership returns the membership row for role/grantRole, whether
// it exists, and any error.
func grantRoleReadMembership(q QueryAble, role, grantRole string) (roleName, grantRoleName string, withAdmin, found bool, err error) {
	err = q.QueryRow(getGrantRoleQuery, role, grantRole).Scan(&roleName, &grantRoleName, &withAdmin)
	switch {
	case err == sql.ErrNoRows:
		return "", "", false, false, nil
	case err != nil:
		return "", "", false, false, fmt.Errorf("error reading grant role: %w", err)
	}
	return roleName, grantRoleName, withAdmin, true, nil
}

// grantRoleGrantQuery builds the GRANT statement for a role membership.
func grantRoleGrantQuery(role, grantRole string, withAdmin bool) string {
	query := fmt.Sprintf(
		"GRANT %s TO %s",
		pq.QuoteIdentifier(grantRole),
		pq.QuoteIdentifier(role),
	)
	if withAdmin {
		query = query + " WITH ADMIN OPTION"
	}
	return query
}

// grantRoleRevokeQuery builds the REVOKE statement for a role membership.
func grantRoleRevokeQuery(role, grantRole string) string {
	return fmt.Sprintf(
		"REVOKE %s FROM %s",
		pq.QuoteIdentifier(grantRole),
		pq.QuoteIdentifier(role),
	)
}

// grantRoleGenerateID builds the synthetic id "role_grantRole_bool".
func grantRoleGenerateID(role, grantRole string, withAdmin bool) string {
	return strings.Join([]string{role, grantRole, strconv.FormatBool(withAdmin)}, "_")
}
