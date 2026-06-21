package postgresql

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/lib/pq"
)

var (
	_ resource.Resource                = (*roleResource)(nil)
	_ resource.ResourceWithConfigure   = (*roleResource)(nil)
	_ resource.ResourceWithImportState = (*roleResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*roleResource)(nil)
)

// NewRoleResource returns the postgresql_role resource.
func NewRoleResource() resource.Resource {
	return &roleResource{}
}

type roleResource struct {
	client *Client
}

type roleResourceModel struct {
	ID                              types.String `tfsdk:"id"`
	Name                            types.String `tfsdk:"name"`
	Password                        types.String `tfsdk:"password"`
	PasswordWO                      types.String `tfsdk:"password_wo"`
	PasswordWOVersion               types.String `tfsdk:"password_wo_version"`
	Encrypted                       types.String `tfsdk:"encrypted"`
	Roles                           types.Set    `tfsdk:"roles"`
	SearchPath                      types.List   `tfsdk:"search_path"`
	EncryptedPassword               types.Bool   `tfsdk:"encrypted_password"`
	ValidUntil                      types.String `tfsdk:"valid_until"`
	ConnectionLimit                 types.Int64  `tfsdk:"connection_limit"`
	Superuser                       types.Bool   `tfsdk:"superuser"`
	CreateDatabase                  types.Bool   `tfsdk:"create_database"`
	CreateRole                      types.Bool   `tfsdk:"create_role"`
	IdleInTransactionSessionTimeout types.Int64  `tfsdk:"idle_in_transaction_session_timeout"`
	Inherit                         types.Bool   `tfsdk:"inherit"`
	Login                           types.Bool   `tfsdk:"login"`
	Replication                     types.Bool   `tfsdk:"replication"`
	BypassRLS                       types.Bool   `tfsdk:"bypass_row_level_security"`
	SkipDropRole                    types.Bool   `tfsdk:"skip_drop_role"`
	SkipReassignOwned               types.Bool   `tfsdk:"skip_reassign_owned"`
	StatementTimeout                types.Int64  `tfsdk:"statement_timeout"`
	AssumeRole                      types.String `tfsdk:"assume_role"`
}

// ModifyPlan keeps the computed id in sync with name. The role name is updatable
// in place (ALTER ROLE ... RENAME), and the id equals the name, so on a rename
// the planned id must change too — otherwise the id plan modifier
// (UseStateForUnknown) would keep the old value while apply writes the new one,
// yielding a "provider produced inconsistent result" error.
func (r *roleResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		// Resource is being destroyed.
		return
	}
	var name types.String
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("name"), &name)...)
	if resp.Diagnostics.HasError() || name.IsUnknown() || name.IsNull() {
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("id"), name)...)
}

func (r *roleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_role"
}

func (r *roleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	emptyStringSet := types.SetValueMust(types.StringType, []attr.Value{})
	emptyStringList := types.ListValueMust(types.StringType, []attr.Value{})

	resp.Schema = schema.Schema{
		Description: "Creates and manages a role on a PostgreSQL server.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Synthetic id: the role name",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "The name of the role",
			},
			"password": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "Sets the role's password",
				Validators: []validator.String{
					stringvalidator.ConflictsWith(
						path.MatchRoot("password_wo"),
						path.MatchRoot("password_wo_version"),
					),
				},
			},
			"password_wo": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				WriteOnly:   true,
				Description: "Sets the role's password without storing it in the state file.",
				Validators: []validator.String{
					stringvalidator.ConflictsWith(path.MatchRoot("password")),
					stringvalidator.AlsoRequires(path.MatchRoot("password_wo_version")),
				},
			},
			"password_wo_version": schema.StringAttribute{
				Optional:    true,
				Description: "Prevents applies from updating the role password on every apply unless the value changes.",
				Validators: []validator.String{
					stringvalidator.ConflictsWith(path.MatchRoot("password")),
					stringvalidator.AlsoRequires(path.MatchRoot("password_wo")),
				},
			},
			"encrypted": schema.StringAttribute{
				Optional:           true,
				DeprecationMessage: "Rename PostgreSQL role resource attribute \"encrypted\" to \"encrypted_password\"",
			},
			"roles": schema.SetAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Computed:    true,
				Default:     setdefault.StaticValue(emptyStringSet),
				Description: "Role(s) to grant to this new role",
			},
			"search_path": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Computed:    true,
				Default:     listdefault.StaticValue(emptyStringList),
				Description: "Sets the role's search path",
			},
			"encrypted_password": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(true),
				Description: "Control whether the password is stored encrypted in the system catalogs",
			},
			"valid_until": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString("infinity"),
				Description: "Sets a date and time after which the role's password is no longer valid",
			},
			"connection_limit": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Default:     int64default.StaticInt64(-1),
				Description: "How many concurrent connections can be made with this role",
				Validators:  []validator.Int64{int64validator.AtLeast(-1)},
			},
			"superuser": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: `Determine whether the new role is a "superuser"`,
			},
			"create_database": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "Define a role's ability to create databases",
			},
			"create_role": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "Determine whether this role will be permitted to create new roles",
			},
			"idle_in_transaction_session_timeout": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Default:     int64default.StaticInt64(0),
				Description: "Terminate any session with an open transaction that has been idle for longer than the specified duration in milliseconds",
				Validators:  []validator.Int64{int64validator.AtLeast(0)},
			},
			"inherit": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(true),
				Description: `Determine whether a role "inherits" the privileges of roles it is a member of`,
			},
			"login": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "Determine whether a role is allowed to log in",
			},
			"replication": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "Determine whether a role is allowed to initiate streaming replication or put the system in and out of backup mode",
			},
			"bypass_row_level_security": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "Determine whether a role bypasses every row-level security (RLS) policy",
			},
			"skip_drop_role": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "Skip actually running the DROP ROLE command when removing a ROLE from PostgreSQL",
			},
			"skip_reassign_owned": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "Skip actually running the REASSIGN OWNED command when removing a role from PostgreSQL",
			},
			"statement_timeout": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Default:     int64default.StaticInt64(0),
				Description: "Abort any statement that takes more than the specified number of milliseconds",
				Validators:  []validator.Int64{int64validator.AtLeast(0)},
			},
			"assume_role": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString(""),
				Description: "Role to switch to at login",
			},
		},
	}
}

func (r *roleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// roleFwWriteOnlyPassword reads the write-only password_wo attribute from the
// configuration (it is never present in plan/state). It returns ("", false) when
// the value is unset, unknown or empty.
func roleFwWriteOnlyPassword(ctx context.Context, cfg tfsdk.Config, diags *diag.Diagnostics) (string, bool) {
	var pw types.String
	diags.Append(cfg.GetAttribute(ctx, path.Root("password_wo"), &pw)...)
	if diags.HasError() || pw.IsNull() || pw.IsUnknown() || pw.ValueString() == "" {
		return "", false
	}
	return pw.ValueString(), true
}

func roleFwStringSlice(ctx context.Context, set types.Set, diags *diag.Diagnostics) []string {
	var out []string
	if set.IsNull() || set.IsUnknown() {
		return out
	}
	diags.Append(set.ElementsAs(ctx, &out, false)...)
	return out
}

func roleFwStringList(ctx context.Context, list types.List, diags *diag.Diagnostics) []string {
	var out []string
	if list.IsNull() || list.IsUnknown() {
		return out
	}
	diags.Append(list.ElementsAs(ctx, &out, false)...)
	return out
}

func (r *roleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data roleResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	passwordWO, woSet := roleFwWriteOnlyPassword(ctx, req.Config, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	db, err := r.client.Connect()
	if err != nil {
		resp.Diagnostics.AddError("could not connect to database", err.Error())
		return
	}

	txn, err := startTransaction(r.client, "")
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	roleName := data.Name.ValueString()

	createOpts := []string{}

	// Password (regular value stored in state takes precedence over write-only).
	var pwVal string
	pwSet := false
	if !data.Password.IsNull() && !data.Password.IsUnknown() && data.Password.ValueString() != "" {
		pwVal = data.Password.ValueString()
		pwSet = true
	} else if woSet {
		pwVal = passwordWO
		pwSet = true
	}
	if pwSet {
		if strings.ToUpper(pwVal) == "NULL" {
			createOpts = append(createOpts, "PASSWORD NULL")
		} else {
			if data.EncryptedPassword.ValueBool() {
				createOpts = append(createOpts, "ENCRYPTED")
			} else {
				createOpts = append(createOpts, "UNENCRYPTED")
			}
			createOpts = append(createOpts, fmt.Sprintf("PASSWORD '%s'", pqQuoteLiteral(pwVal)))
		}
	}

	// VALID UNTIL is always set (default "infinity").
	validUntil := data.ValidUntil.ValueString()
	if validUntil == "" || strings.ToLower(validUntil) == "infinity" {
		createOpts = append(createOpts, "VALID UNTIL 'infinity'")
	} else {
		createOpts = append(createOpts, fmt.Sprintf("VALID UNTIL '%s'", pqQuoteLiteral(validUntil)))
	}

	createOpts = append(createOpts, fmt.Sprintf("CONNECTION LIMIT %d", data.ConnectionLimit.ValueInt64()))

	type roleBoolOpt struct {
		val             bool
		enable, disable string
	}
	boolOpts := []roleBoolOpt{
		{data.Superuser.ValueBool(), "SUPERUSER", "NOSUPERUSER"},
		{data.CreateDatabase.ValueBool(), "CREATEDB", "NOCREATEDB"},
		{data.CreateRole.ValueBool(), "CREATEROLE", "NOCREATEROLE"},
		{data.Inherit.ValueBool(), "INHERIT", "NOINHERIT"},
		{data.Login.ValueBool(), "LOGIN", "NOLOGIN"},
	}
	if db.featureSupported(featureRLS) {
		boolOpts = append(boolOpts, roleBoolOpt{data.BypassRLS.ValueBool(), "BYPASSRLS", "NOBYPASSRLS"})
	}
	if db.featureSupported(featureReplication) {
		boolOpts = append(boolOpts, roleBoolOpt{data.Replication.ValueBool(), "REPLICATION", "NOREPLICATION"})
	}
	for _, opt := range boolOpts {
		if opt.val {
			createOpts = append(createOpts, opt.enable)
		} else {
			createOpts = append(createOpts, opt.disable)
		}
	}

	createStr := strings.Join(createOpts, " ")
	if len(createOpts) > 0 {
		if db.featureSupported(featureCreateRoleWith) {
			createStr = " WITH " + createStr
		} else {
			// NOTE(seanc@): Work around ParAccel/AWS RedShift's ancient fork of PostgreSQL
			createStr = " " + createStr
		}
	}

	sqlStr := fmt.Sprintf("CREATE ROLE %s%s", pq.QuoteIdentifier(roleName), createStr)
	if _, err := txn.Exec(sqlStr); err != nil {
		resp.Diagnostics.AddError("could not create role", fmt.Sprintf("error creating role %s: %v", roleName, err))
		return
	}

	if err := roleFwGrantRoles(txn, roleName, roleFwStringSlice(ctx, data.Roles, &resp.Diagnostics)); err != nil {
		resp.Diagnostics.AddError("could not grant roles", err.Error())
		return
	}
	if resp.Diagnostics.HasError() {
		return
	}

	if err := roleFwAlterSearchPath(txn, roleName, roleFwStringList(ctx, data.SearchPath, &resp.Diagnostics)); err != nil {
		resp.Diagnostics.AddError("could not set search_path", err.Error())
		return
	}
	if resp.Diagnostics.HasError() {
		return
	}

	// statement_timeout / idle_in_transaction_session_timeout: on create only
	// apply when non-zero.
	if st := data.StatementTimeout.ValueInt64(); st != 0 {
		if err := roleFwSetStatementTimeout(txn, roleName, st); err != nil {
			resp.Diagnostics.AddError("could not set statement_timeout", err.Error())
			return
		}
	}
	if idle := data.IdleInTransactionSessionTimeout.ValueInt64(); idle != 0 {
		if err := roleFwSetIdleInTransactionSessionTimeout(txn, roleName, idle); err != nil {
			resp.Diagnostics.AddError("could not set idle_in_transaction_session_timeout", err.Error())
			return
		}
	}
	if assume := data.AssumeRole.ValueString(); assume != "" {
		if err := roleFwSetAssumeRole(txn, roleName, assume); err != nil {
			resp.Diagnostics.AddError("could not set assume_role", err.Error())
			return
		}
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("could not commit transaction", err.Error())
		return
	}

	data.ID = types.StringValue(roleName)

	if found := r.readRoleInto(ctx, db, &data, &resp.Diagnostics); resp.Diagnostics.HasError() {
		return
	} else if !found {
		resp.Diagnostics.AddError("could not create role", "role not found after creation")
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *roleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data roleResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	db, err := r.client.Connect()
	if err != nil {
		resp.Diagnostics.AddError("could not connect to database", err.Error())
		return
	}

	found := r.readRoleInto(ctx, db, &data, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// readRoleInto reads the role attributes from the server into data. It returns
// false if the role no longer exists. The id is used as the lookup key (it
// equals the name).
func (r *roleResource) readRoleInto(ctx context.Context, db *DBConnection, data *roleResourceModel, diags *diag.Diagnostics) bool {
	var roleSuperuser, roleInherit, roleCreateRole, roleCreateDB, roleCanLogin, roleReplication, roleBypassRLS bool
	var roleConnLimit int
	var roleName, roleValidUntil string
	var roleRoles, roleConfig pq.ByteaArray

	roleID := data.ID.ValueString()
	if roleID == "" {
		roleID = data.Name.ValueString()
	}

	columns := []string{
		"rolname",
		"rolsuper",
		"rolinherit",
		"rolcreaterole",
		"rolcreatedb",
		"rolcanlogin",
		"rolconnlimit",
		`COALESCE(rolvaliduntil::TEXT, 'infinity')`,
		"rolconfig",
	}

	values := []any{
		&roleRoles,
		&roleName,
		&roleSuperuser,
		&roleInherit,
		&roleCreateRole,
		&roleCreateDB,
		&roleCanLogin,
		&roleConnLimit,
		&roleValidUntil,
		&roleConfig,
	}

	if db.featureSupported(featureReplication) {
		columns = append(columns, "rolreplication")
		values = append(values, &roleReplication)
	}

	if db.featureSupported(featureRLS) {
		columns = append(columns, "rolbypassrls")
		values = append(values, &roleBypassRLS)
	}

	roleSQL := fmt.Sprintf(`SELECT ARRAY(
			SELECT pg_get_userbyid(roleid) FROM pg_catalog.pg_auth_members members WHERE member = pg_roles.oid
		), %s
		FROM pg_catalog.pg_roles WHERE rolname=$1`,
		strings.Join(columns, ", "),
	)
	err := db.QueryRow(roleSQL, roleID).Scan(values...)

	switch {
	case err == sql.ErrNoRows:
		log.Printf("[WARN] PostgreSQL ROLE (%s) not found", roleID)
		return false
	case err != nil:
		diags.AddError("error reading ROLE", err.Error())
		return false
	}

	data.Name = types.StringValue(roleName)
	data.ConnectionLimit = types.Int64Value(int64(roleConnLimit))
	data.CreateDatabase = types.BoolValue(roleCreateDB)
	data.CreateRole = types.BoolValue(roleCreateRole)
	data.EncryptedPassword = types.BoolValue(true)
	data.Inherit = types.BoolValue(roleInherit)
	data.Login = types.BoolValue(roleCanLogin)
	data.Superuser = types.BoolValue(roleSuperuser)
	data.ValidUntil = types.StringValue(roleValidUntil)
	data.Replication = types.BoolValue(roleReplication)
	data.BypassRLS = types.BoolValue(roleBypassRLS)

	rolesSet, d := types.SetValueFrom(ctx, types.StringType, byteaArrayToStringSlice(roleRoles))
	diags.Append(d...)
	if diags.HasError() {
		return false
	}
	data.Roles = rolesSet

	// search_path: when the role has no explicit search_path entry in rolconfig
	// it is left as an empty list. Returning an empty list — not a null/non-empty
	// value — is required so the applied value stays consistent with the planned
	// empty list.
	var searchPathList types.List
	if searchPathSlice := roleFwReadSearchPath(roleConfig); len(searchPathSlice) == 0 {
		searchPathList = types.ListValueMust(types.StringType, []attr.Value{})
	} else {
		var spd diag.Diagnostics
		searchPathList, spd = types.ListValueFrom(ctx, types.StringType, searchPathSlice)
		diags.Append(spd...)
		if diags.HasError() {
			return false
		}
	}
	data.SearchPath = searchPathList

	data.AssumeRole = types.StringValue(roleFwReadAssumeRole(roleConfig))

	statementTimeout, err := roleFwReadStatementTimeout(roleConfig)
	if err != nil {
		diags.AddError("error reading statement_timeout", err.Error())
		return false
	}
	data.StatementTimeout = types.Int64Value(int64(statementTimeout))

	idleInTransactionSessionTimeout, err := roleFwReadIdleInTransactionSessionTimeout(roleConfig)
	if err != nil {
		diags.AddError("error reading idle_in_transaction_session_timeout", err.Error())
		return false
	}
	data.IdleInTransactionSessionTimeout = types.Int64Value(int64(idleInTransactionSessionTimeout))

	data.ID = types.StringValue(roleName)

	if !data.Password.IsNull() && !data.Password.IsUnknown() && data.Password.ValueString() != "" {
		password, err := roleFwReadRolePassword(db, roleName, data.Password.ValueString(), roleCanLogin)
		if err != nil {
			diags.AddError("error reading role password", err.Error())
			return false
		}
		data.Password = types.StringValue(password)
	}

	return true
}

func (r *roleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state roleResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	passwordWO, woSet := roleFwWriteOnlyPassword(ctx, req.Config, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	db, err := r.client.Connect()
	if err != nil {
		resp.Diagnostics.AddError("could not connect to database", err.Error())
		return
	}

	txn, err := startTransaction(r.client, "")
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	oldName := state.Name.ValueString()
	if err := pgLockRole(txn, oldName); err != nil {
		resp.Diagnostics.AddError("could not lock role", err.Error())
		return
	}

	roleName := plan.Name.ValueString()

	// Rename the role if its name changed.
	if roleName != oldName {
		if roleName == "" {
			resp.Diagnostics.AddError("invalid role name", "error setting role name to an empty string")
			return
		}
		stmt := fmt.Sprintf("ALTER ROLE %s RENAME TO %s", pq.QuoteIdentifier(oldName), pq.QuoteIdentifier(roleName))
		if _, err := txn.Exec(stmt); err != nil {
			resp.Diagnostics.AddError("error updating role NAME", err.Error())
			return
		}
	}

	// Password.
	if err := roleFwSetRolePassword(txn, plan, state, passwordWO, woSet); err != nil {
		resp.Diagnostics.AddError("error updating role password", err.Error())
		return
	}

	// BypassRLS.
	if plan.BypassRLS.ValueBool() != state.BypassRLS.ValueBool() {
		if !db.featureSupported(featureRLS) {
			resp.Diagnostics.AddError("feature not supported", fmt.Sprintf("PostgreSQL client is talking with a server (%q) that does not support PostgreSQL Row-Level Security", db.version.String()))
			return
		}
		if err := roleFwAlterBool(txn, roleName, plan.BypassRLS.ValueBool(), "BYPASSRLS", "NOBYPASSRLS", "BYPASSRLS"); err != nil {
			resp.Diagnostics.AddError("error updating role", err.Error())
			return
		}
	}

	if plan.ConnectionLimit.ValueInt64() != state.ConnectionLimit.ValueInt64() {
		stmt := fmt.Sprintf("ALTER ROLE %s CONNECTION LIMIT %d", pq.QuoteIdentifier(roleName), plan.ConnectionLimit.ValueInt64())
		if _, err := txn.Exec(stmt); err != nil {
			resp.Diagnostics.AddError("error updating role CONNECTION LIMIT", err.Error())
			return
		}
	}

	if plan.CreateDatabase.ValueBool() != state.CreateDatabase.ValueBool() {
		if err := roleFwAlterBool(txn, roleName, plan.CreateDatabase.ValueBool(), "CREATEDB", "NOCREATEDB", "CREATEDB"); err != nil {
			resp.Diagnostics.AddError("error updating role", err.Error())
			return
		}
	}

	if plan.CreateRole.ValueBool() != state.CreateRole.ValueBool() {
		if err := roleFwAlterBool(txn, roleName, plan.CreateRole.ValueBool(), "CREATEROLE", "NOCREATEROLE", "CREATEROLE"); err != nil {
			resp.Diagnostics.AddError("error updating role", err.Error())
			return
		}
	}

	if plan.Inherit.ValueBool() != state.Inherit.ValueBool() {
		if err := roleFwAlterBool(txn, roleName, plan.Inherit.ValueBool(), "INHERIT", "NOINHERIT", "INHERIT"); err != nil {
			resp.Diagnostics.AddError("error updating role", err.Error())
			return
		}
	}

	if plan.Login.ValueBool() != state.Login.ValueBool() {
		if err := roleFwAlterBool(txn, roleName, plan.Login.ValueBool(), "LOGIN", "NOLOGIN", "LOGIN"); err != nil {
			resp.Diagnostics.AddError("error updating role", err.Error())
			return
		}
	}

	if plan.Replication.ValueBool() != state.Replication.ValueBool() {
		if err := roleFwAlterBool(txn, roleName, plan.Replication.ValueBool(), "REPLICATION", "NOREPLICATION", "REPLICATION"); err != nil {
			resp.Diagnostics.AddError("error updating role", err.Error())
			return
		}
	}

	if plan.Superuser.ValueBool() != state.Superuser.ValueBool() {
		if err := roleFwAlterBool(txn, roleName, plan.Superuser.ValueBool(), "SUPERUSER", "NOSUPERUSER", "SUPERUSER"); err != nil {
			resp.Diagnostics.AddError("error updating role", err.Error())
			return
		}
	}

	if plan.ValidUntil.ValueString() != state.ValidUntil.ValueString() {
		validUntil := plan.ValidUntil.ValueString()
		if validUntil != "" {
			if strings.ToLower(validUntil) == "infinity" {
				validUntil = "infinity"
			}
			stmt := fmt.Sprintf("ALTER ROLE %s VALID UNTIL '%s'", pq.QuoteIdentifier(roleName), pqQuoteLiteral(validUntil))
			if _, err := txn.Exec(stmt); err != nil {
				resp.Diagnostics.AddError("error updating role VALID UNTIL", err.Error())
				return
			}
		}
	}

	// Roles: revoke everything then grant the wanted set.
	if err := roleFwRevokeRoles(txn, roleName); err != nil {
		resp.Diagnostics.AddError("could not revoke roles", err.Error())
		return
	}
	if err := roleFwGrantRoles(txn, roleName, roleFwStringSlice(ctx, plan.Roles, &resp.Diagnostics)); err != nil {
		resp.Diagnostics.AddError("could not grant roles", err.Error())
		return
	}
	if resp.Diagnostics.HasError() {
		return
	}

	// search_path is always re-applied.
	if err := roleFwAlterSearchPath(txn, roleName, roleFwStringList(ctx, plan.SearchPath, &resp.Diagnostics)); err != nil {
		resp.Diagnostics.AddError("could not set search_path", err.Error())
		return
	}
	if resp.Diagnostics.HasError() {
		return
	}

	if plan.StatementTimeout.ValueInt64() != state.StatementTimeout.ValueInt64() {
		if st := plan.StatementTimeout.ValueInt64(); st != 0 {
			if err := roleFwSetStatementTimeout(txn, roleName, st); err != nil {
				resp.Diagnostics.AddError("could not set statement_timeout", err.Error())
				return
			}
		} else {
			if err := roleFwResetStatementTimeout(txn, roleName); err != nil {
				resp.Diagnostics.AddError("could not reset statement_timeout", err.Error())
				return
			}
		}
	}

	if plan.IdleInTransactionSessionTimeout.ValueInt64() != state.IdleInTransactionSessionTimeout.ValueInt64() {
		if idle := plan.IdleInTransactionSessionTimeout.ValueInt64(); idle != 0 {
			if err := roleFwSetIdleInTransactionSessionTimeout(txn, roleName, idle); err != nil {
				resp.Diagnostics.AddError("could not set idle_in_transaction_session_timeout", err.Error())
				return
			}
		} else {
			if err := roleFwResetIdleInTransactionSessionTimeout(txn, roleName); err != nil {
				resp.Diagnostics.AddError("could not reset idle_in_transaction_session_timeout", err.Error())
				return
			}
		}
	}

	if plan.AssumeRole.ValueString() != state.AssumeRole.ValueString() {
		if assume := plan.AssumeRole.ValueString(); assume != "" {
			if err := roleFwSetAssumeRole(txn, roleName, assume); err != nil {
				resp.Diagnostics.AddError("could not set assume_role", err.Error())
				return
			}
		} else {
			if err := roleFwResetAssumeRole(txn, roleName); err != nil {
				resp.Diagnostics.AddError("could not reset assume_role", err.Error())
				return
			}
		}
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("could not commit transaction", err.Error())
		return
	}

	data := plan
	data.ID = types.StringValue(roleName)

	if found := r.readRoleInto(ctx, db, &data, &resp.Diagnostics); resp.Diagnostics.HasError() {
		return
	} else if !found {
		resp.State.RemoveResource(ctx)
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *roleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data roleResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if _, err := r.client.Connect(); err != nil {
		resp.Diagnostics.AddError("could not connect to database", err.Error())
		return
	}

	roleName := data.Name.ValueString()

	txn, err := startTransaction(r.client, "")
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	if err := pgLockRole(txn, roleName); err != nil {
		resp.Diagnostics.AddError("could not lock role", err.Error())
		return
	}

	if !data.SkipReassignOwned.ValueBool() {
		if err := withRolesGranted(txn, []string{roleName}, func() error {
			currentUser := r.client.config.getDatabaseUsername()
			if _, err := txn.Exec(fmt.Sprintf("REASSIGN OWNED BY %s TO %s", pq.QuoteIdentifier(roleName), pq.QuoteIdentifier(currentUser))); err != nil {
				return fmt.Errorf("could not reassign owned by role %s to %s: %w", roleName, currentUser, err)
			}
			if _, err := txn.Exec(fmt.Sprintf("DROP OWNED BY %s", pq.QuoteIdentifier(roleName))); err != nil {
				return fmt.Errorf("could not drop owned by role %s: %w", roleName, err)
			}
			return nil
		}); err != nil {
			resp.Diagnostics.AddError("could not reassign/drop owned objects", err.Error())
			return
		}
	}

	if !data.SkipDropRole.ValueBool() {
		if _, err := txn.Exec(fmt.Sprintf("DROP ROLE %s", pq.QuoteIdentifier(roleName))); err != nil {
			resp.Diagnostics.AddError("could not delete role", fmt.Sprintf("could not delete role %s: %v", roleName, err))
			return
		}
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("error committing transaction", err.Error())
		return
	}
}

// ImportState accepts the role name (which is also the id).
func (r *roleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

// roleFwSetRolePassword applies the role password. woSet/passwordWO carry the
// write-only password value read from the configuration.
func roleFwSetRolePassword(txn *sql.Tx, plan, state roleResourceModel, passwordWO string, woSet bool) error {
	if woSet {
		// Only re-apply the write-only password when its version changed.
		if plan.PasswordWOVersion.ValueString() == state.PasswordWOVersion.ValueString() {
			return nil
		}
	} else {
		// Only for the regular password attribute: exit if neither password nor
		// role name changed.
		if plan.Password.ValueString() == state.Password.ValueString() &&
			plan.Name.ValueString() == state.Name.ValueString() {
			return nil
		}
	}

	roleName := plan.Name.ValueString()

	var password string
	if woSet {
		password = passwordWO
	} else if !plan.Password.IsNull() && !plan.Password.IsUnknown() && plan.Password.ValueString() != "" {
		password = plan.Password.ValueString()
	} else {
		// Nothing to set.
		return nil
	}

	stmt := fmt.Sprintf("ALTER ROLE %s PASSWORD '%s'", pq.QuoteIdentifier(roleName), pqQuoteLiteral(password))
	if _, err := txn.Exec(stmt); err != nil {
		return fmt.Errorf("error updating role password: %w", err)
	}
	return nil
}

func roleFwAlterBool(txn *sql.Tx, roleName string, val bool, enable, disable, label string) error {
	tok := disable
	if val {
		tok = enable
	}
	stmt := fmt.Sprintf("ALTER ROLE %s WITH %s", pq.QuoteIdentifier(roleName), tok)
	if _, err := txn.Exec(stmt); err != nil {
		return fmt.Errorf("error updating role %s: %w", label, err)
	}
	return nil
}

func roleFwRevokeRoles(txn *sql.Tx, role string) error {
	query := `SELECT pg_get_userbyid(roleid)
		FROM pg_catalog.pg_auth_members members
		JOIN pg_catalog.pg_roles ON members.member = pg_roles.oid
		WHERE rolname = $1`

	rows, err := txn.Query(query, role)
	if err != nil {
		return fmt.Errorf("could not get roles list for role %s: %w", role, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			log.Printf("error closing rows: %v", err)
		}
	}()

	grantedRoles := []string{}
	for rows.Next() {
		var grantedRole string
		if err = rows.Scan(&grantedRole); err != nil {
			return fmt.Errorf("could not scan role name for role %s: %w", role, err)
		}
		// We cannot revoke directly here as it shares the same cursor (with Tx)
		// and rows.Next seems to retrieve result row by row.
		// see: https://github.com/lib/pq/issues/81
		grantedRoles = append(grantedRoles, grantedRole)
	}

	for _, grantedRole := range grantedRoles {
		query := fmt.Sprintf("REVOKE %s FROM %s", pq.QuoteIdentifier(grantedRole), pq.QuoteIdentifier(role))
		if _, err := txn.Exec(query); err != nil {
			return fmt.Errorf("could not revoke role %s from %s: %w", grantedRole, role, err)
		}
	}

	return nil
}

func roleFwGrantRoles(txn *sql.Tx, role string, roles []string) error {
	for _, grantingRole := range roles {
		query := fmt.Sprintf("GRANT %s TO %s", pq.QuoteIdentifier(grantingRole), pq.QuoteIdentifier(role))
		if _, err := txn.Exec(query); err != nil {
			return fmt.Errorf("could not grant role %s to %s: %w", grantingRole, role, err)
		}
	}
	return nil
}

func roleFwAlterSearchPath(txn *sql.Tx, role string, searchPathInterface []string) error {
	// When no search_path is managed, RESET it so the role keeps NO explicit
	// search_path entry in pg_roles.rolconfig. RESET removes the entry rather than
	// persisting a "DEFAULT" value — which would otherwise be read back as a
	// non-empty list and break plan/apply consistency.
	if len(searchPathInterface) == 0 {
		query := fmt.Sprintf("ALTER ROLE %s RESET search_path", pq.QuoteIdentifier(role))
		if _, err := txn.Exec(query); err != nil {
			return fmt.Errorf("could not reset search_path for %s: %w", role, err)
		}
		return nil
	}

	searchPathString := make([]string, len(searchPathInterface))
	for i, searchPathPart := range searchPathInterface {
		if strings.Contains(searchPathPart, ", ") {
			return fmt.Errorf("search_path cannot contain `, `: %v", searchPathPart)
		}
		searchPathString[i] = pq.QuoteIdentifier(searchPathPart)
	}
	searchPath := strings.Join(searchPathString, ", ")

	query := fmt.Sprintf("ALTER ROLE %s SET search_path TO %s", pq.QuoteIdentifier(role), searchPath)
	if _, err := txn.Exec(query); err != nil {
		return fmt.Errorf("could not set search_path %s for %s: %w", searchPath, role, err)
	}
	return nil
}

func roleFwSetStatementTimeout(txn *sql.Tx, roleName string, statementTimeout int64) error {
	stmt := fmt.Sprintf("ALTER ROLE %s SET statement_timeout TO %d", pq.QuoteIdentifier(roleName), statementTimeout)
	if _, err := txn.Exec(stmt); err != nil {
		return fmt.Errorf("could not set statement_timeout %d for %s: %w", statementTimeout, roleName, err)
	}
	return nil
}

func roleFwResetStatementTimeout(txn *sql.Tx, roleName string) error {
	stmt := fmt.Sprintf("ALTER ROLE %s RESET statement_timeout", pq.QuoteIdentifier(roleName))
	if _, err := txn.Exec(stmt); err != nil {
		return fmt.Errorf("could not reset statement_timeout for %s: %w", roleName, err)
	}
	return nil
}

func roleFwSetIdleInTransactionSessionTimeout(txn *sql.Tx, roleName string, idleInTransactionSessionTimeout int64) error {
	stmt := fmt.Sprintf("ALTER ROLE %s SET idle_in_transaction_session_timeout TO %d", pq.QuoteIdentifier(roleName), idleInTransactionSessionTimeout)
	if _, err := txn.Exec(stmt); err != nil {
		return fmt.Errorf("could not set idle_in_transaction_session_timeout %d for %s: %w", idleInTransactionSessionTimeout, roleName, err)
	}
	return nil
}

func roleFwResetIdleInTransactionSessionTimeout(txn *sql.Tx, roleName string) error {
	stmt := fmt.Sprintf("ALTER ROLE %s RESET idle_in_transaction_session_timeout", pq.QuoteIdentifier(roleName))
	if _, err := txn.Exec(stmt); err != nil {
		return fmt.Errorf("could not reset idle_in_transaction_session_timeout for %s: %w", roleName, err)
	}
	return nil
}

func roleFwSetAssumeRole(txn *sql.Tx, roleName, assumeRole string) error {
	stmt := fmt.Sprintf("ALTER ROLE %s SET ROLE TO %s", pq.QuoteIdentifier(roleName), pq.QuoteIdentifier(assumeRole))
	if _, err := txn.Exec(stmt); err != nil {
		return fmt.Errorf("could not set role %s for %s: %w", assumeRole, roleName, err)
	}
	return nil
}

func roleFwResetAssumeRole(txn *sql.Tx, roleName string) error {
	stmt := fmt.Sprintf("ALTER ROLE %s RESET ROLE", pq.QuoteIdentifier(roleName))
	if _, err := txn.Exec(stmt); err != nil {
		return fmt.Errorf("could not reset role for %s: %w", roleName, err)
	}
	return nil
}

// roleFwReadSearchPath searches for a search_path entry in the rolconfig array.
// In case no such value is present (the role has no explicit search_path), it
// returns nil. A "DEFAULT" sentinel value is also treated as unset so the role's
// default search_path normalizes to an empty list.
func roleFwReadSearchPath(roleConfig pq.ByteaArray) []string {
	for _, v := range roleConfig {
		config := string(v)
		if strings.HasPrefix(config, "search_path=") {
			value := strings.TrimPrefix(config, "search_path=")
			if value == "" || strings.EqualFold(value, "DEFAULT") {
				return nil
			}
			result := strings.Split(value, ", ")
			for i := range result {
				result[i] = strings.Trim(result[i], `"`)
			}
			return result
		}
	}
	return nil
}

// roleFwReadIdleInTransactionSessionTimeout searches for an
// idle_in_transaction_session_timeout entry in the rolconfig array.
func roleFwReadIdleInTransactionSessionTimeout(roleConfig pq.ByteaArray) (int, error) {
	for _, v := range roleConfig {
		config := string(v)
		if strings.HasPrefix(config, "idle_in_transaction_session_timeout") {
			result := strings.Split(strings.TrimPrefix(config, "idle_in_transaction_session_timeout="), ", ")
			res, err := strconv.Atoi(result[0])
			if err != nil {
				return -1, fmt.Errorf("error reading statement_timeout: %w", err)
			}
			return res, nil
		}
	}
	return 0, nil
}

// roleFwReadStatementTimeout searches for a statement_timeout entry in the
// rolconfig array.
func roleFwReadStatementTimeout(roleConfig pq.ByteaArray) (int, error) {
	for _, v := range roleConfig {
		config := string(v)
		if strings.HasPrefix(config, "statement_timeout") {
			result := strings.Split(strings.TrimPrefix(config, "statement_timeout="), ", ")
			res, err := strconv.Atoi(result[0])
			if err != nil {
				return -1, fmt.Errorf("error reading statement_timeout: %w", err)
			}
			return res, nil
		}
	}
	return 0, nil
}

// roleFwReadAssumeRole searches for a role entry in the rolconfig array.
func roleFwReadAssumeRole(roleConfig pq.ByteaArray) string {
	var res string
	assumeRoleAttr := "role"
	for _, v := range roleConfig {
		config := string(v)
		if strings.HasPrefix(config, assumeRoleAttr) {
			res = strings.TrimPrefix(config, assumeRoleAttr+"=")
		}
	}
	return res
}

// roleFwReadRolePassword reads the password either from Postgres if the admin
// user is a superuser or only from the Terraform state.
func roleFwReadRolePassword(db *DBConnection, roleName, statePassword string, roleCanLogin bool) (string, error) {
	// Role which cannot login does not have password in pg_shadow.
	// Also, if user specifies that admin is not a superuser we don't try to read
	// pg_shadow (only superuser can read pg_shadow).
	if !roleCanLogin || !db.client.config.Superuser {
		return statePassword, nil
	}

	// Otherwise we check if connected user is really a superuser (in order to
	// warn user instead of having a permission denied error).
	superuser, err := db.isSuperuser()
	if err != nil {
		return "", err
	}
	if !superuser {
		return "", fmt.Errorf(
			"could not read role password from Postgres as "+
				"connected user %s is not a SUPERUSER. "+
				"You can set `superuser = false` in the provider configuration "+
				"so it will not try to read the password from Postgres",
			db.client.config.getDatabaseUsername(),
		)
	}

	var rolePassword string
	err = db.QueryRow("SELECT COALESCE(passwd, '') FROM pg_catalog.pg_shadow AS s WHERE s.usename = $1", roleName).Scan(&rolePassword)
	switch {
	case err == sql.ErrNoRows:
		// They don't have a password.
		return "", nil
	case err != nil:
		return "", fmt.Errorf("error reading role: %w", err)
	}
	// If the password isn't already in md5 format, but hashing the input matches
	// the password in the database for the user, they are the same.
	if statePassword != "" && !strings.HasPrefix(statePassword, "md5") && !strings.HasPrefix(statePassword, "SCRAM-SHA-256") {
		if strings.HasPrefix(rolePassword, "md5") {
			hasher := md5.New()
			if _, err := hasher.Write([]byte(statePassword + roleName)); err != nil {
				return "", err
			}
			hashedPassword := "md5" + hex.EncodeToString(hasher.Sum(nil))

			if hashedPassword == rolePassword {
				// The passwords are actually the same, make Terraform think they
				// are the same.
				return statePassword, nil
			}
		}
		if strings.HasPrefix(rolePassword, "SCRAM-SHA-256") {
			return statePassword, nil
			// TODO : implement scram-sha-256 challenge request to the server
		}
	}
	return rolePassword, nil
}
