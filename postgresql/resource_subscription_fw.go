package postgresql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/lib/pq"
)

var (
	_ resource.Resource                = (*subscriptionResource)(nil)
	_ resource.ResourceWithConfigure   = (*subscriptionResource)(nil)
	_ resource.ResourceWithImportState = (*subscriptionResource)(nil)
)

// NewSubscriptionResource returns the postgresql_subscription resource.
func NewSubscriptionResource() resource.Resource {
	return &subscriptionResource{}
}

type subscriptionResource struct {
	client *Client
}

type subscriptionResourceModel struct {
	ID           types.String `tfsdk:"id"`
	Name         types.String `tfsdk:"name"`
	Database     types.String `tfsdk:"database"`
	ConnInfo     types.String `tfsdk:"conninfo"`
	Publications types.Set    `tfsdk:"publications"`
	CreateSlot   types.Bool   `tfsdk:"create_slot"`
	SlotName     types.String `tfsdk:"slot_name"`
}

func (r *subscriptionResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_subscription"
}

func (r *subscriptionResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a PostgreSQL logical replication subscription.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Synthetic id: database.name",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:      true,
				Description:   "The name of the subscription",
				Validators:    []validator.String{stringvalidator.LengthAtLeast(1)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"database": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Sets the database to add the subscription for",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"conninfo": schema.StringAttribute{
				Required:      true,
				Sensitive:     true,
				Description:   "The connection string to the publisher. It should follow the keyword/value format (https://www.postgresql.org/docs/current/libpq-connect.html#LIBPQ-CONNSTRING)",
				Validators:    []validator.String{stringvalidator.LengthAtLeast(1)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"publications": schema.SetAttribute{
				ElementType:   types.StringType,
				Required:      true,
				Description:   "Names of the publications on the publisher to subscribe to",
				PlanModifiers: []planmodifier.Set{setplanmodifier.RequiresReplace()},
			},
			"create_slot": schema.BoolAttribute{
				Optional:      true,
				Computed:      true,
				Default:       booldefault.StaticBool(true),
				Description:   "Specifies whether the command should create the replication slot on the publisher",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},
			"slot_name": schema.StringAttribute{
				Optional:      true,
				Description:   "Name of the replication slot to use. The default behavior is to use the name of the subscription for the slot name",
				Validators:    []validator.String{stringvalidator.LengthAtLeast(1)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
		},
	}
}

func (r *subscriptionResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// resolveDatabase returns the database attribute if set (non-empty), otherwise
// the provider's default database.
func (r *subscriptionResource) resolveDatabase(m subscriptionResourceModel) string {
	if m.Database.IsNull() || m.Database.IsUnknown() || m.Database.ValueString() == "" {
		return r.client.DatabaseName()
	}
	return m.Database.ValueString()
}

func subscriptionID(database, name string) string {
	return strings.Join([]string{database, name}, ".")
}

// subscriptionPublicationsString quotes and joins publication names, rejecting
// duplicates.
func subscriptionPublicationsString(publications []string) (string, error) {
	if len(publications) == 0 {
		return "", fmt.Errorf("attribute publications is not set")
	}
	seen := make(map[string]bool, len(publications))
	plist := make([]string, 0, len(publications))
	for _, p := range publications {
		if seen[p] {
			return "", fmt.Errorf("'%s' is duplicated for attribute publications", p)
		}
		seen[p] = true
		plist = append(plist, pq.QuoteIdentifier(p))
	}
	return strings.Join(plist, ", "), nil
}

// subscriptionOptionalParameters emits a WITH (...) clause only when create_slot
// and/or slot_name were explicitly set.
func subscriptionOptionalParameters(createSlotSet, createSlot, slotNameSet bool, slotName string) string {
	if !createSlotSet && !slotNameSet {
		return ""
	}
	var params []string
	if createSlotSet {
		params = append(params, fmt.Sprintf("%s = %t", "create_slot", createSlot))
	}
	if slotNameSet {
		params = append(params, fmt.Sprintf("%s = %s", "slot_name", pq.QuoteLiteral(slotName)))
	}
	return fmt.Sprintf("WITH (%s)", strings.Join(params, ", "))
}

func (r *subscriptionResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan, config subscriptionResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	subName := plan.Name.ValueString()
	database := r.resolveDatabase(plan)

	var pubNames []string
	resp.Diagnostics.Append(plan.Publications.ElementsAs(ctx, &pubNames, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	publications, err := subscriptionPublicationsString(pubNames)
	if err != nil {
		resp.Diagnostics.AddError("could not get publications", err.Error())
		return
	}

	connInfo := plan.ConnInfo.ValueString()
	if connInfo == "" {
		resp.Diagnostics.AddError("could not get conninfo", "attribute conninfo is not set")
		return
	}

	// create_slot has a static default (true), so the plan value is always known;
	// use the raw config to detect whether the attribute was explicitly set.
	createSlotSet := !config.CreateSlot.IsNull()
	slotNameSet := !config.SlotName.IsNull() && config.SlotName.ValueString() != ""
	optionalParams := subscriptionOptionalParameters(
		createSlotSet, plan.CreateSlot.ValueBool(),
		slotNameSet, plan.SlotName.ValueString(),
	)

	// Creating a subscription can not be done in a transaction.
	conn, err := r.client.ForDatabase(database).Connect()
	if err != nil {
		resp.Diagnostics.AddError("could not establish database connection", err.Error())
		return
	}

	createSQL := fmt.Sprintf("CREATE SUBSCRIPTION %s CONNECTION %s PUBLICATION %s %s;",
		pq.QuoteIdentifier(subName),
		pq.QuoteLiteral(connInfo),
		publications,
		optionalParams,
	)
	if _, err := conn.Exec(createSQL); err != nil {
		resp.Diagnostics.AddError("could not execute sql", err.Error())
		return
	}

	plan.Database = types.StringValue(database)
	plan.ID = types.StringValue(subscriptionID(database, subName))

	found := r.read(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.Diagnostics.AddError("could not read subscription", "subscription was not found right after creation")
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *subscriptionResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data subscriptionResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	found := r.read(ctx, &data, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// read refreshes the model in place from the database. It returns false if the
// subscription (or its database) no longer exists.
func (r *subscriptionResource) read(ctx context.Context, data *subscriptionResourceModel, diags *diag.Diagnostics) bool {
	database := r.resolveDatabase(*data)
	subName := data.Name.ValueString()

	// Check that the database exists.
	conn, err := r.client.Connect()
	if err != nil {
		diags.AddError("could not connect to database", err.Error())
		return false
	}
	exists, err := dbExists(conn, database)
	if err != nil {
		diags.AddError("could not check if database exists", err.Error())
		return false
	}
	if !exists {
		return false
	}

	txn, err := startTransaction(r.client, database)
	if err != nil {
		diags.AddError("could not start transaction", err.Error())
		return false
	}
	defer deferredRollback(txn)

	// Existence check.
	var found string
	err = txn.QueryRow(
		"SELECT subname from pg_catalog.pg_stat_subscription WHERE subname = $1",
		pqQuoteLiteral(subName),
	).Scan(&found)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false
	case err != nil:
		diags.AddError("failed to check subscription", err.Error())
		return false
	}

	// pg_subscription requires superuser permissions, it is okay to fail here.
	var publications []string
	var connInfo string
	var slotName string
	query := "SELECT subconninfo, subpublications, subslotname FROM pg_catalog.pg_subscription WHERE subname = $1"
	err = txn.QueryRow(query, pqQuoteLiteral(subName)).Scan(&connInfo, pq.Array(&publications), &slotName)
	if err != nil {
		// We already checked that the subscription exists, fall back to the
		// values configured in state.
		if data.ConnInfo.IsNull() || data.ConnInfo.ValueString() == "" {
			diags.AddError("could not get conninfo", "attribute conninfo is not set")
			return false
		}
		if data.Publications.IsNull() {
			diags.AddError("could not get publications", "attribute publications is not set")
			return false
		}
		// data.ConnInfo and data.Publications are kept as-is.
	} else {
		data.ConnInfo = types.StringValue(connInfo)
		pubSet, d := types.SetValueFrom(ctx, types.StringType, publications)
		diags.Append(d...)
		if diags.HasError() {
			return false
		}
		data.Publications = pubSet

		// slot_name is only tracked when it was explicitly configured.
		if !data.SlotName.IsNull() && data.SlotName.ValueString() != "" {
			data.SlotName = types.StringValue(slotName)
		}
	}

	data.Name = types.StringValue(subName)
	data.Database = types.StringValue(database)
	data.ID = types.StringValue(subscriptionID(database, subName))

	return true
}

// Update never makes changes (all attributes force replacement); it persists the plan.
func (r *subscriptionResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data subscriptionResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *subscriptionResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data subscriptionResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	subName := data.Name.ValueString()
	createSlot := data.CreateSlot.ValueBool()
	database := r.resolveDatabase(data)

	// Dropping a subscription can not be done in a transaction.
	conn, err := r.client.ForDatabase(database).Connect()
	if err != nil {
		resp.Diagnostics.AddError("could not establish database connection", err.Error())
		return
	}

	// Disable the subscription and unset the slot before dropping in order to
	// keep the replication slot.
	if !createSlot {
		disableSQL := fmt.Sprintf("ALTER SUBSCRIPTION %s DISABLE", pq.QuoteIdentifier(subName))
		if _, err := conn.Exec(disableSQL); err != nil {
			resp.Diagnostics.AddError("could not execute sql", err.Error())
			return
		}
		unsetSlotSQL := fmt.Sprintf("ALTER SUBSCRIPTION %s SET (slot_name = NONE)", pq.QuoteIdentifier(subName))
		if _, err := conn.Exec(unsetSlotSQL); err != nil {
			resp.Diagnostics.AddError("could not execute sql", err.Error())
			return
		}
	}

	dropSQL := fmt.Sprintf("DROP SUBSCRIPTION %s", pq.QuoteIdentifier(subName))
	if _, err := conn.Exec(dropSQL); err != nil {
		resp.Diagnostics.AddError("could not execute sql", err.Error())
		return
	}
}

// ImportState accepts "database.name" (the subscription ID format).
func (r *subscriptionResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(req.ID, ".")
	if len(parts) != 2 {
		resp.Diagnostics.AddError(
			"invalid import ID",
			fmt.Sprintf("subscription ID %s has not the expected format 'database.subscriptionName'", req.ID),
		)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("database"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
