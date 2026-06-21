package postgresql

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/lib/pq"
)

var (
	_ resource.Resource                = (*serverResource)(nil)
	_ resource.ResourceWithConfigure   = (*serverResource)(nil)
	_ resource.ResourceWithImportState = (*serverResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*serverResource)(nil)
)

// ModifyPlan keeps the computed id in sync with server_name. server_name is
// updatable in place (ALTER SERVER ... RENAME), and the id equals the server
// name, so on a rename the planned id must change too — otherwise the id plan
// modifier (UseStateForUnknown) would keep the old value while apply writes the
// new one, yielding a "provider produced inconsistent result" error.
func (r *serverResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		// Resource is being destroyed.
		return
	}
	var serverName types.String
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("server_name"), &serverName)...)
	if resp.Diagnostics.HasError() || serverName.IsUnknown() || serverName.IsNull() {
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("id"), serverName)...)
}

// NewServerResource returns the postgresql_server resource.
func NewServerResource() resource.Resource {
	return &serverResource{}
}

type serverResource struct {
	client *Client
}

type serverResourceModel struct {
	ID            types.String `tfsdk:"id"`
	ServerName    types.String `tfsdk:"server_name"`
	ServerType    types.String `tfsdk:"server_type"`
	ServerVersion types.String `tfsdk:"server_version"`
	ServerOwner   types.String `tfsdk:"server_owner"`
	FDWName       types.String `tfsdk:"fdw_name"`
	Options       types.Map    `tfsdk:"options"`
	DropCascade   types.Bool   `tfsdk:"drop_cascade"`
}

func (r *serverResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_server"
}

func (r *serverResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a PostgreSQL foreign server.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Synthetic id: the foreign server name",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"server_name": schema.StringAttribute{
				Required:    true,
				Description: "The name of the foreign server to be created",
			},
			"server_type": schema.StringAttribute{
				Optional:      true,
				Description:   "Optional server type, potentially useful to foreign-data wrappers",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"server_version": schema.StringAttribute{
				Optional:    true,
				Description: "Optional server version, potentially useful to foreign-data wrappers.",
			},
			"fdw_name": schema.StringAttribute{
				Required:      true,
				Description:   "The name of the foreign-data wrapper that manages the server",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"server_owner": schema.StringAttribute{
				Optional:      true,
				Computed:      true,
				Description:   "The user name of the new owner of the foreign server",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"options": schema.MapAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "This clause specifies the options for the server. The options typically define the connection details of the server, but the actual names and values are dependent on the server's foreign-data wrapper",
			},
			"drop_cascade": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "Automatically drop objects that depend on the server (such as user mappings), and in turn all objects that depend on those objects. Drop RESTRICT is the default",
			},
		},
	}
}

func (r *serverResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// connect opens a DB connection and verifies the foreign server feature is
// supported by the connected PostgreSQL version.
func (r *serverResource) connect(diags *diag.Diagnostics) *DBConnection {
	db, err := r.client.Connect()
	if err != nil {
		diags.AddError("could not connect to database", err.Error())
		return nil
	}
	if !db.featureSupported(featureServer) {
		diags.AddError(
			"feature not supported",
			fmt.Sprintf("foreign server resource is not supported for this Postgres version (%s)", db.version),
		)
		return nil
	}
	return db
}

func (r *serverResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data serverResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if r.connect(&resp.Diagnostics); resp.Diagnostics.HasError() {
		return
	}

	serverName := data.ServerName.ValueString()

	options, d := userMappingOptionsFromMap(ctx, data.Options)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}

	b := bytes.NewBufferString("CREATE SERVER ")
	fmt.Fprint(b, pq.QuoteIdentifier(serverName))

	if !data.ServerType.IsNull() && !data.ServerType.IsUnknown() && data.ServerType.ValueString() != "" {
		fmt.Fprint(b, " TYPE ", pq.QuoteLiteral(data.ServerType.ValueString()))
	}

	if !data.ServerVersion.IsNull() && !data.ServerVersion.IsUnknown() && data.ServerVersion.ValueString() != "" {
		fmt.Fprint(b, " VERSION ", pq.QuoteLiteral(data.ServerVersion.ValueString()))
	}

	fmt.Fprint(b, " FOREIGN DATA WRAPPER ", pq.QuoteIdentifier(data.FDWName.ValueString()))

	if len(options) > 0 {
		fmt.Fprint(b, " OPTIONS ( ")
		cnt := 0
		n := len(options)
		for k, v := range options {
			fmt.Fprint(b, " ", pq.QuoteIdentifier(k), " ", pq.QuoteLiteral(v))
			if cnt < n-1 {
				fmt.Fprint(b, ", ")
			}
			cnt++
		}
		fmt.Fprint(b, " ) ")
	}

	txn, err := startTransaction(r.client, "")
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	if _, err := txn.Exec(b.String()); err != nil {
		resp.Diagnostics.AddError("could not create foreign server", err.Error())
		return
	}

	// Set the owner if it was explicitly provided and differs from the current user.
	if !data.ServerOwner.IsNull() && !data.ServerOwner.IsUnknown() && data.ServerOwner.ValueString() != "" {
		currentUser, err := getCurrentUser(txn)
		if err != nil {
			resp.Diagnostics.AddError("could not get current user", err.Error())
			return
		}
		if data.ServerOwner.ValueString() != currentUser {
			if err := serverSetOwner(txn, serverName, data.ServerOwner.ValueString()); err != nil {
				resp.Diagnostics.AddError("could not set foreign server owner", err.Error())
				return
			}
		}
	}

	// Read back the (computed) owner so it gets a concrete value in state.
	info, exists, err := serverReadInfo(txn, serverName)
	if err != nil {
		resp.Diagnostics.AddError("error reading foreign server", err.Error())
		return
	}
	if !exists {
		resp.Diagnostics.AddError("could not create foreign server", "foreign server not found after creation")
		return
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("error creating server", err.Error())
		return
	}

	data.ServerOwner = types.StringValue(info.owner)
	data.ID = types.StringValue(serverName)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *serverResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data serverResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if r.connect(&resp.Diagnostics); resp.Diagnostics.HasError() {
		return
	}

	serverName := data.ServerName.ValueString()

	txn, err := startTransaction(r.client, "")
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	info, exists, err := serverReadInfo(txn, serverName)
	if err != nil {
		resp.Diagnostics.AddError("error reading foreign server", err.Error())
		return
	}
	if !exists {
		resp.State.RemoveResource(ctx)
		return
	}

	data.ServerName = types.StringValue(serverName)
	if info.serverType == "" {
		data.ServerType = types.StringNull()
	} else {
		data.ServerType = types.StringValue(info.serverType)
	}
	if info.serverVersion == "" {
		data.ServerVersion = types.StringNull()
	} else {
		data.ServerVersion = types.StringValue(info.serverVersion)
	}
	data.ServerOwner = types.StringValue(info.owner)
	data.FDWName = types.StringValue(info.fdwName)

	if len(info.options) == 0 {
		data.Options = types.MapNull(types.StringType)
	} else {
		mv, d := types.MapValueFrom(ctx, types.StringType, info.options)
		resp.Diagnostics.Append(d...)
		if resp.Diagnostics.HasError() {
			return
		}
		data.Options = mv
	}

	data.ID = types.StringValue(serverName)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *serverResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state serverResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if r.connect(&resp.Diagnostics); resp.Diagnostics.HasError() {
		return
	}

	txn, err := startTransaction(r.client, "")
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	serverName := plan.ServerName.ValueString()

	// Rename the server if its name changed.
	if plan.ServerName.ValueString() != state.ServerName.ValueString() {
		stmt := fmt.Sprintf("ALTER SERVER %s RENAME TO %s",
			pq.QuoteIdentifier(state.ServerName.ValueString()),
			pq.QuoteIdentifier(plan.ServerName.ValueString()),
		)
		if _, err := txn.Exec(stmt); err != nil {
			resp.Diagnostics.AddError("error updating foreign server name", err.Error())
			return
		}
	}

	// Update the owner if it changed.
	if plan.ServerOwner.ValueString() != state.ServerOwner.ValueString() {
		if err := serverSetOwner(txn, serverName, plan.ServerOwner.ValueString()); err != nil {
			resp.Diagnostics.AddError("error updating foreign server owner", err.Error())
			return
		}
	}

	// Update version and/or options if changed.
	oldOptions, d1 := userMappingOptionsFromMap(ctx, state.Options)
	resp.Diagnostics.Append(d1...)
	newOptions, d2 := userMappingOptionsFromMap(ctx, plan.Options)
	resp.Diagnostics.Append(d2...)
	if resp.Diagnostics.HasError() {
		return
	}

	versionChanged := plan.ServerVersion.ValueString() != state.ServerVersion.ValueString()
	optionsChanged := !userMappingOptionsEqual(oldOptions, newOptions)

	if versionChanged || optionsChanged {
		b := bytes.NewBufferString("ALTER SERVER ")
		fmt.Fprintf(b, "%s ", pq.QuoteIdentifier(serverName))

		if versionChanged {
			fmt.Fprintf(b, "VERSION %s", pq.QuoteLiteral(plan.ServerVersion.ValueString()))
		}

		if optionsChanged {
			fmt.Fprint(b, " OPTIONS ( ")
			cnt := 0
			n := len(newOptions)
			toRemove := oldOptions
			for k, v := range newOptions {
				operation := "ADD"
				if _, ok := oldOptions[k]; ok {
					operation = "SET"
					delete(toRemove, k)
				}
				fmt.Fprintf(b, " %s %s %s ", operation, pq.QuoteIdentifier(k), pq.QuoteLiteral(v))
				if cnt < n-1 {
					fmt.Fprint(b, ", ")
				}
				cnt++
			}

			for k := range toRemove {
				if cnt != 0 { // starting with 0 means to drop all the options. Cannot start with comma
					fmt.Fprint(b, " , ")
				}
				fmt.Fprintf(b, " DROP %s ", pq.QuoteIdentifier(k))
				cnt++
			}

			fmt.Fprint(b, " ) ")
		}

		if _, err := txn.Exec(b.String()); err != nil {
			resp.Diagnostics.AddError("error updating foreign server version and/or options", err.Error())
			return
		}
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("error updating foreign server", err.Error())
		return
	}

	plan.ID = types.StringValue(serverName)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *serverResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data serverResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if r.connect(&resp.Diagnostics); resp.Diagnostics.HasError() {
		return
	}

	serverName := data.ServerName.ValueString()

	txn, err := startTransaction(r.client, "")
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	dropMode := "RESTRICT"
	if data.DropCascade.ValueBool() {
		dropMode = "CASCADE"
	}

	stmt := fmt.Sprintf("DROP SERVER %s %s ", pq.QuoteIdentifier(serverName), dropMode)
	if _, err := txn.Exec(stmt); err != nil {
		resp.Diagnostics.AddError("could not drop foreign server", err.Error())
		return
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("error deleting server", err.Error())
	}
}

// ImportState accepts the foreign server name (which is also the id).
func (r *serverResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("server_name"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

// serverInfo holds the data read back from pg_foreign_server.
type serverInfo struct {
	serverType    string
	serverVersion string
	owner         string
	fdwName       string
	options       map[string]string
}

// serverReadInfo reads a foreign server's attributes, returning whether the
// server exists and any error.
func serverReadInfo(txn *sql.Tx, serverName string) (*serverInfo, bool, error) {
	var serverType, serverVersion, serverOwner, serverFDW string
	var serverOptions []string
	query := `SELECT COALESCE(fs.srvtype, ''), COALESCE(fs.srvversion, ''), fs.srvowner::regrole, fs.srvoptions, w.fdwname ` +
		`FROM pg_foreign_server fs JOIN pg_foreign_data_wrapper w on w.oid = fs.srvfdw ` +
		`WHERE fs.srvname = $1`
	err := txn.QueryRow(query, serverName).Scan(&serverType, &serverVersion, &serverOwner, pq.Array(&serverOptions), &serverFDW)
	switch {
	case err == sql.ErrNoRows:
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("error reading foreign server: %w", err)
	}

	mappedOptions := make(map[string]string)
	for _, v := range serverOptions {
		pair := strings.Split(v, "=")
		mappedOptions[pair[0]] = pair[1]
	}

	return &serverInfo{
		serverType:    serverType,
		serverVersion: serverVersion,
		owner:         serverOwner,
		fdwName:       serverFDW,
		options:       mappedOptions,
	}, true, nil
}

// serverSetOwner issues ALTER SERVER ... OWNER TO ... within the given txn.
func serverSetOwner(txn *sql.Tx, serverName, owner string) error {
	stmt := fmt.Sprintf("ALTER SERVER %s OWNER TO %s", pq.QuoteIdentifier(serverName), pq.QuoteIdentifier(owner))
	if _, err := txn.Exec(stmt); err != nil {
		return fmt.Errorf("error updating foreign server owner: %w", err)
	}
	return nil
}
