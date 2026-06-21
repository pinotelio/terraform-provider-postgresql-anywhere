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
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/lib/pq"
)

var (
	_ resource.Resource                = (*userMappingResource)(nil)
	_ resource.ResourceWithConfigure   = (*userMappingResource)(nil)
	_ resource.ResourceWithImportState = (*userMappingResource)(nil)
)

// NewUserMappingResource returns the postgresql_user_mapping resource.
func NewUserMappingResource() resource.Resource {
	return &userMappingResource{}
}

type userMappingResource struct {
	client *Client
}

type userMappingResourceModel struct {
	ID         types.String `tfsdk:"id"`
	UserName   types.String `tfsdk:"user_name"`
	ServerName types.String `tfsdk:"server_name"`
	Options    types.Map    `tfsdk:"options"`
}

func (r *userMappingResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user_mapping"
}

func (r *userMappingResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a PostgreSQL user mapping for a foreign server.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Synthetic id: user_name.server_name",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"user_name": schema.StringAttribute{
				Required:      true,
				Description:   "The name of an existing user that is mapped to foreign server. CURRENT_ROLE, CURRENT_USER, and USER match the name of the current user. When PUBLIC is specified, a so-called public mapping is created that is used when no user-specific mapping is applicable",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"server_name": schema.StringAttribute{
				Required:      true,
				Description:   "The name of an existing server for which the user mapping is to be created",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"options": schema.MapAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "This clause specifies the options of the user mapping. The options typically define the actual user name and password of the mapping. Option names must be unique. The allowed option names and values are specific to the server's foreign-data wrapper",
			},
		},
	}
}

func (r *userMappingResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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
func (r *userMappingResource) connect(diags *diag.Diagnostics) *DBConnection {
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

func (r *userMappingResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data userMappingResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	db := r.connect(&resp.Diagnostics)
	if db == nil {
		return
	}

	username := data.UserName.ValueString()
	serverName := data.ServerName.ValueString()

	options, d := userMappingOptionsFromMap(ctx, data.Options)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}

	b := bytes.NewBufferString("CREATE USER MAPPING ")
	fmt.Fprint(b, " FOR ", pq.QuoteIdentifier(username))
	fmt.Fprint(b, " SERVER ", pq.QuoteIdentifier(serverName))

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

	if _, err := db.Exec(b.String()); err != nil {
		resp.Diagnostics.AddError("could not create user mapping", err.Error())
		return
	}

	data.ID = types.StringValue(username + "." + serverName)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *userMappingResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data userMappingResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	db := r.connect(&resp.Diagnostics)
	if db == nil {
		return
	}

	username := data.UserName.ValueString()
	serverName := data.ServerName.ValueString()

	txn, err := startTransaction(r.client, "")
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	mapped, exists, err := readUserMappingOptions(txn, username, serverName)
	if err != nil {
		resp.Diagnostics.AddError("error reading user mapping", err.Error())
		return
	}
	if !exists {
		resp.State.RemoveResource(ctx)
		return
	}

	if len(mapped) == 0 {
		data.Options = types.MapNull(types.StringType)
	} else {
		mv, d := types.MapValueFrom(ctx, types.StringType, mapped)
		resp.Diagnostics.Append(d...)
		if resp.Diagnostics.HasError() {
			return
		}
		data.Options = mv
	}

	data.ID = types.StringValue(username + "." + serverName)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *userMappingResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state userMappingResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	db := r.connect(&resp.Diagnostics)
	if db == nil {
		return
	}

	username := plan.UserName.ValueString()
	serverName := plan.ServerName.ValueString()

	oldOptions, d1 := userMappingOptionsFromMap(ctx, state.Options)
	resp.Diagnostics.Append(d1...)
	newOptions, d2 := userMappingOptionsFromMap(ctx, plan.Options)
	resp.Diagnostics.Append(d2...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !userMappingOptionsEqual(oldOptions, newOptions) {
		b := bytes.NewBufferString("ALTER USER MAPPING ")
		fmt.Fprintf(b, " FOR %s SERVER %s ", pq.QuoteIdentifier(username), pq.QuoteIdentifier(serverName))

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

		if _, err := db.Exec(b.String()); err != nil {
			resp.Diagnostics.AddError("error updating user mapping options", err.Error())
			return
		}
	}

	plan.ID = types.StringValue(username + "." + serverName)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *userMappingResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data userMappingResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	db := r.connect(&resp.Diagnostics)
	if db == nil {
		return
	}

	username := data.UserName.ValueString()
	serverName := data.ServerName.ValueString()

	txn, err := startTransaction(r.client, "")
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	stmt := fmt.Sprintf("DROP USER MAPPING FOR %s SERVER %s ", pq.QuoteIdentifier(username), pq.QuoteIdentifier(serverName))
	if _, err := txn.Exec(stmt); err != nil {
		resp.Diagnostics.AddError("could not drop user mapping", err.Error())
		return
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("error deleting user mapping", err.Error())
	}
}

// ImportState accepts "user_name.server_name".
func (r *userMappingResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, ".", 2)
	if len(parts) != 2 {
		resp.Diagnostics.AddError("invalid import ID", "expected format \"user_name.server_name\"")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("user_name"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("server_name"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

// readUserMappingOptions returns the options of the user mapping for the given
// user/server, whether it exists, and any error. It falls back from
// information_schema._pg_user_mappings to pg_user_mappings.
func readUserMappingOptions(txn *sql.Tx, username, serverName string) (map[string]string, bool, error) {
	var userMappingOptions []string
	query := "SELECT umoptions FROM information_schema._pg_user_mappings WHERE authorization_identifier = $1 and foreign_server_name = $2"
	err := txn.QueryRow(query, username, serverName).Scan(pq.Array(&userMappingOptions))

	if err != sql.ErrNoRows && err != nil {
		// Fallback to pg_user_mappings table if information_schema._pg_user_mappings is not available
		query = "SELECT umoptions FROM pg_user_mappings WHERE usename = $1 and srvname = $2"
		err = txn.QueryRow(query, username, serverName).Scan(pq.Array(&userMappingOptions))
	}

	switch {
	case err == sql.ErrNoRows:
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("error reading user mapping: %w", err)
	}

	mapped := make(map[string]string)
	for _, v := range userMappingOptions {
		pair := strings.SplitN(v, "=", 2)
		mapped[pair[0]] = pair[1]
	}

	return mapped, true, nil
}

// userMappingOptionsFromMap converts a framework string map into a Go map,
// treating null/unknown as empty.
func userMappingOptionsFromMap(ctx context.Context, m types.Map) (map[string]string, diag.Diagnostics) {
	result := make(map[string]string)
	if m.IsNull() || m.IsUnknown() {
		return result, nil
	}
	diags := m.ElementsAs(ctx, &result, false)
	return result, diags
}

func userMappingOptionsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
