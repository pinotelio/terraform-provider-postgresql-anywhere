package postgresql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*physicalReplicationSlotResource)(nil)
	_ resource.ResourceWithConfigure   = (*physicalReplicationSlotResource)(nil)
	_ resource.ResourceWithImportState = (*physicalReplicationSlotResource)(nil)
)

// NewPhysicalReplicationSlotResource returns the
// postgresql_physical_replication_slot resource.
func NewPhysicalReplicationSlotResource() resource.Resource {
	return &physicalReplicationSlotResource{}
}

type physicalReplicationSlotResource struct {
	client *Client
}

type physicalReplicationSlotModel struct {
	ID   types.String `tfsdk:"id"`
	Name types.String `tfsdk:"name"`
}

func (r *physicalReplicationSlotResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_physical_replication_slot"
}

func (r *physicalReplicationSlotResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a PostgreSQL physical replication slot.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Synthetic id: the physical replication slot name.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:      true,
				Description:   "Name of the physical replication slot.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
		},
	}
}

func (r *physicalReplicationSlotResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *physicalReplicationSlotResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data physicalReplicationSlotModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	name := data.Name.ValueString()

	txn, err := startTransaction(r.client, r.client.DatabaseName())
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	if _, err := txn.Exec("SELECT FROM pg_create_physical_replication_slot($1)", name); err != nil {
		resp.Diagnostics.AddError("could not create physical replication slot", fmt.Sprintf("could not create physical ReplicationSlot %s: %v", name, err))
		return
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("could not commit physical replication slot", err.Error())
		return
	}

	data.ID = types.StringValue(name)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *physicalReplicationSlotResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data physicalReplicationSlotModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// The id is the canonical identifier; the name is derived from it.
	name := data.ID.ValueString()
	if name == "" {
		name = data.Name.ValueString()
	}

	txn, err := startTransaction(r.client, r.client.DatabaseName())
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	var unused int
	err = txn.QueryRow(
		"SELECT 1 FROM pg_catalog.pg_replication_slots WHERE slot_name = $1 and slot_type = 'physical'",
		name,
	).Scan(&unused)
	if errors.Is(err, sql.ErrNoRows) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("could not read physical replication slot", err.Error())
		return
	}

	data.Name = types.StringValue(name)
	data.ID = types.StringValue(name)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// Update never makes changes (the name attribute forces replacement); it
// persists the plan.
func (r *physicalReplicationSlotResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data physicalReplicationSlotModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *physicalReplicationSlotResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data physicalReplicationSlotModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	txn, err := startTransaction(r.client, r.client.DatabaseName())
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	if _, err := txn.Exec("SELECT pg_drop_replication_slot($1)", data.Name.ValueString()); err != nil {
		resp.Diagnostics.AddError("could not drop physical replication slot", err.Error())
		return
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("could not commit physical replication slot deletion", err.Error())
	}
}

// ImportState accepts the physical replication slot name as the id.
func (r *physicalReplicationSlotResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
