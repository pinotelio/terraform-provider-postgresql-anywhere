package postgresql

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/lib/pq"
)

var (
	_ resource.Resource                = (*securityLabelResource)(nil)
	_ resource.ResourceWithConfigure   = (*securityLabelResource)(nil)
	_ resource.ResourceWithImportState = (*securityLabelResource)(nil)
)

const (
	securityLabelObjectNameAttr = "object_name"
	securityLabelObjectTypeAttr = "object_type"
	securityLabelProviderAttr   = "label_provider"
	securityLabelLabelAttr      = "label"
)

// NewSecurityLabelResource returns the postgresql_security_label resource.
func NewSecurityLabelResource() resource.Resource {
	return &securityLabelResource{}
}

type securityLabelResource struct {
	client *Client
}

type securityLabelModel struct {
	ID            types.String `tfsdk:"id"`
	ObjectName    types.String `tfsdk:"object_name"`
	ObjectType    types.String `tfsdk:"object_type"`
	LabelProvider types.String `tfsdk:"label_provider"`
	Label         types.String `tfsdk:"label"`
}

func (r *securityLabelResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_security_label"
}

func (r *securityLabelResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a PostgreSQL security label.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Synthetic id: label_provider.object_type.object_name",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			securityLabelObjectNameAttr: schema.StringAttribute{
				Required:      true,
				Description:   "The name of the existing object to apply the security label to",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			securityLabelObjectTypeAttr: schema.StringAttribute{
				Required:      true,
				Description:   "The type of the existing object to apply the security label to",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			securityLabelProviderAttr: schema.StringAttribute{
				Required:      true,
				Description:   "The provider to apply the security label for",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			securityLabelLabelAttr: schema.StringAttribute{
				Required:    true,
				Description: "The label to be applied",
			},
		},
	}
}

func (r *securityLabelResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// securityLabelQuoteIdentifier quotes identifiers that are not already a simple,
// all-lowercase identifier.
func securityLabelQuoteIdentifier(s string) string {
	var result = s
	re := regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	if !re.MatchString(s) || s != strings.ToLower(s) {
		result = pq.QuoteIdentifier(s)
	}
	return result
}

// buildSecurityLabelStatement constructs the SECURITY LABEL statement. label
// must already be the SQL-ready value (a quoted literal or the keyword NULL).
func buildSecurityLabelStatement(objectType, objectName, provider, label string) string {
	b := bytes.NewBufferString("SECURITY LABEL ")
	fmt.Fprint(b, " FOR ", pq.QuoteIdentifier(provider))
	fmt.Fprint(b, " ON ", objectType, pq.QuoteIdentifier(objectName))
	fmt.Fprint(b, " IS ", label)
	return b.String()
}

// readSecurityLabel reads the security label back from pg_seclabels and updates
// the model in place. It returns false if no matching security label exists.
func (r *securityLabelResource) readSecurityLabel(db *DBConnection, data *securityLabelModel) (bool, error) {
	objectType := data.ObjectType.ValueString()
	objectName := data.ObjectName.ValueString()
	provider := data.LabelProvider.ValueString()

	txn, err := startTransaction(db.client, "")
	if err != nil {
		return false, err
	}
	defer deferredRollback(txn)

	query := "SELECT objtype, provider, objname, label FROM pg_seclabels WHERE objtype = $1 and objname = $2 and provider = $3"
	row := db.QueryRow(query, objectType, securityLabelQuoteIdentifier(objectName), securityLabelQuoteIdentifier(provider))

	var label, newObjectName, newProvider string
	err = row.Scan(&objectType, &newProvider, &newObjectName, &label)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("error reading security label: %w", err)
	}

	if securityLabelQuoteIdentifier(objectName) != newObjectName || securityLabelQuoteIdentifier(provider) != newProvider {
		// In reality, this should never happen, but if it does we want to make
		// sure that the state is in sync with the remote system.
		objectName = newObjectName
		provider = newProvider
	}

	data.ObjectType = types.StringValue(objectType)
	data.ObjectName = types.StringValue(objectName)
	data.LabelProvider = types.StringValue(provider)
	data.Label = types.StringValue(label)
	data.ID = types.StringValue(strings.Join([]string{provider, objectType, objectName}, "."))

	return true, nil
}

func (r *securityLabelResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data securityLabelModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	db, err := r.client.Connect()
	if err != nil {
		resp.Diagnostics.AddError("could not connect to database", err.Error())
		return
	}

	if !db.featureSupported(featureSecurityLabel) {
		resp.Diagnostics.AddError(
			"security label not supported",
			fmt.Sprintf("security Label is not supported for this Postgres version (%s)", db.version),
		)
		return
	}

	stmt := buildSecurityLabelStatement(
		data.ObjectType.ValueString(),
		data.ObjectName.ValueString(),
		data.LabelProvider.ValueString(),
		pq.QuoteLiteral(data.Label.ValueString()),
	)
	if _, err := db.Exec(stmt); err != nil {
		resp.Diagnostics.AddError("could not create security label", err.Error())
		return
	}

	data.ID = types.StringValue(strings.Join([]string{
		data.LabelProvider.ValueString(),
		data.ObjectType.ValueString(),
		data.ObjectName.ValueString(),
	}, "."))

	if _, err := r.readSecurityLabel(db, &data); err != nil {
		resp.Diagnostics.AddError("could not read security label", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *securityLabelResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data securityLabelModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	db, err := r.client.Connect()
	if err != nil {
		resp.Diagnostics.AddError("could not connect to database", err.Error())
		return
	}

	if !db.featureSupported(featureSecurityLabel) {
		resp.Diagnostics.AddError(
			"security label not supported",
			fmt.Sprintf("security Label is not supported for this Postgres version (%s)", db.version),
		)
		return
	}

	found, err := r.readSecurityLabel(db, &data)
	if err != nil {
		resp.Diagnostics.AddError("could not read security label", err.Error())
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *securityLabelResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data securityLabelModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	db, err := r.client.Connect()
	if err != nil {
		resp.Diagnostics.AddError("could not connect to database", err.Error())
		return
	}

	// NOTE: update intentionally checks featureServer, not featureSecurityLabel.
	if !db.featureSupported(featureServer) {
		resp.Diagnostics.AddError(
			"security label not supported",
			fmt.Sprintf("security Label is not supported for this Postgres version (%s)", db.version),
		)
		return
	}

	stmt := buildSecurityLabelStatement(
		data.ObjectType.ValueString(),
		data.ObjectName.ValueString(),
		data.LabelProvider.ValueString(),
		pq.QuoteLiteral(data.Label.ValueString()),
	)
	if _, err := db.Exec(stmt); err != nil {
		resp.Diagnostics.AddError("could not update security label", err.Error())
		return
	}

	if _, err := r.readSecurityLabel(db, &data); err != nil {
		resp.Diagnostics.AddError("could not read security label", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *securityLabelResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data securityLabelModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	db, err := r.client.Connect()
	if err != nil {
		resp.Diagnostics.AddError("could not connect to database", err.Error())
		return
	}

	if !db.featureSupported(featureSecurityLabel) {
		resp.Diagnostics.AddError(
			"security label not supported",
			fmt.Sprintf("security Label is not supported for this Postgres version (%s)", db.version),
		)
		return
	}

	stmt := buildSecurityLabelStatement(
		data.ObjectType.ValueString(),
		data.ObjectName.ValueString(),
		data.LabelProvider.ValueString(),
		"NULL",
	)
	if _, err := db.Exec(stmt); err != nil {
		resp.Diagnostics.AddError("could not delete security label", err.Error())
		return
	}
}

// ImportState parses the synthetic id "label_provider.object_type.object_name".
func (r *securityLabelResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, ".", 3)
	if len(parts) != 3 {
		resp.Diagnostics.AddError(
			"invalid import ID",
			"expected format \"label_provider.object_type.object_name\"",
		)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root(securityLabelProviderAttr), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root(securityLabelObjectTypeAttr), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root(securityLabelObjectNameAttr), parts[2])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
