package postgresql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*replicationSlotResource)(nil)
	_ resource.ResourceWithConfigure   = (*replicationSlotResource)(nil)
	_ resource.ResourceWithImportState = (*replicationSlotResource)(nil)
)

// NewReplicationSlotResource returns the postgresql_replication_slot resource.
func NewReplicationSlotResource() resource.Resource {
	return &replicationSlotResource{}
}

type replicationSlotResource struct {
	client *Client
}

type replicationSlotModel struct {
	ID       types.String `tfsdk:"id"`
	Name     types.String `tfsdk:"name"`
	Database types.String `tfsdk:"database"`
	Plugin   types.String `tfsdk:"plugin"`
}

func (r *replicationSlotResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_replication_slot"
}

func (r *replicationSlotResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a PostgreSQL logical replication slot.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Synthetic id: database.name",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:      true,
				Description:   "Name of the replication slot.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"database": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Sets the database to add the replication slot to. Defaults to the provider database.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"plugin": schema.StringAttribute{
				Required:      true,
				Description:   "Sets the output plugin to use.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
		},
	}
}

func (r *replicationSlotResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *replicationSlotResource) resolveDatabase(m replicationSlotModel) string {
	if m.Database.IsNull() || m.Database.IsUnknown() || m.Database.ValueString() == "" {
		return r.client.DatabaseName()
	}
	return m.Database.ValueString()
}

func (r *replicationSlotResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data replicationSlotModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	database := r.resolveDatabase(data)

	txn, err := startTransaction(r.client, database)
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	if _, err := txn.Exec(
		"SELECT FROM pg_create_logical_replication_slot($1, $2)",
		data.Name.ValueString(), data.Plugin.ValueString(),
	); err != nil {
		resp.Diagnostics.AddError("could not create replication slot", err.Error())
		return
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("could not commit replication slot", err.Error())
		return
	}

	data.Database = types.StringValue(database)
	data.ID = types.StringValue(fmt.Sprintf("%s.%s", database, data.Name.ValueString()))
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *replicationSlotResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data replicationSlotModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	database := r.resolveDatabase(data)

	txn, err := startTransaction(r.client, database)
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	var plugin string
	err = txn.QueryRow(
		"SELECT plugin FROM pg_catalog.pg_replication_slots WHERE slot_name = $1 AND database = $2",
		data.Name.ValueString(), database,
	).Scan(&plugin)
	if errors.Is(err, sql.ErrNoRows) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("could not read replication slot", err.Error())
		return
	}

	data.Plugin = types.StringValue(plugin)
	data.Database = types.StringValue(database)
	data.ID = types.StringValue(fmt.Sprintf("%s.%s", database, data.Name.ValueString()))
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// Update never makes changes (all attributes force replacement); it persists plan.
func (r *replicationSlotResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data replicationSlotModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *replicationSlotResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data replicationSlotModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	database := r.resolveDatabase(data)

	txn, err := startTransaction(r.client, database)
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	if _, err := txn.Exec("SELECT pg_drop_replication_slot($1)", data.Name.ValueString()); err != nil {
		resp.Diagnostics.AddError("could not drop replication slot", err.Error())
		return
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("could not commit replication slot deletion", err.Error())
	}
}

// ImportState accepts "database.name".
func (r *replicationSlotResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, ".", 2)
	if len(parts) != 2 {
		resp.Diagnostics.AddError("invalid import ID", "expected format \"database.name\"")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("database"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
