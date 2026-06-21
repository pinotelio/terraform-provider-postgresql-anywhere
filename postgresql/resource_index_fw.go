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
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/lib/pq"
)

var (
	_ resource.Resource                = (*indexResource)(nil)
	_ resource.ResourceWithConfigure   = (*indexResource)(nil)
	_ resource.ResourceWithImportState = (*indexResource)(nil)
)

// NewIndexResource returns the postgresql_index resource.
func NewIndexResource() resource.Resource {
	return &indexResource{}
}

type indexResource struct {
	client *Client
}

type indexModel struct {
	ID       types.String `tfsdk:"id"`
	Database types.String `tfsdk:"database"`
	Schema   types.String `tfsdk:"schema"`
	Table    types.String `tfsdk:"table"`
	Name     types.String `tfsdk:"name"`
	Columns  types.List   `tfsdk:"columns"`
	Unique   types.Bool   `tfsdk:"unique"`
	Method   types.String `tfsdk:"method"`
	Where    types.String `tfsdk:"where"`
}

func (r *indexResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_index"
}

func (r *indexResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a PostgreSQL index. Most attributes force replacement, since indexes are not altered in place.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Synthetic id: database.schema.name",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"database": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Database the index lives in. Defaults to the provider database.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"schema": schema.StringAttribute{
				Optional:      true,
				Computed:      true,
				Default:       stringdefault.StaticString("public"),
				Description:   "Schema of the table.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"table": schema.StringAttribute{
				Required:      true,
				Description:   "Table the index is created on.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				Required:      true,
				Description:   "Name of the index.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"columns": schema.ListAttribute{
				ElementType:   types.StringType,
				Required:      true,
				Description:   "Columns the index covers, in order.",
				PlanModifiers: []planmodifier.List{listplanmodifier.RequiresReplace()},
			},
			"unique": schema.BoolAttribute{
				Optional:      true,
				Computed:      true,
				Default:       booldefault.StaticBool(false),
				Description:   "Whether the index is UNIQUE.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},
			"method": schema.StringAttribute{
				Optional:      true,
				Computed:      true,
				Default:       stringdefault.StaticString("btree"),
				Description:   "Index method: btree, hash, gin, gist, spgist, brin.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"where": schema.StringAttribute{
				Optional:      true,
				Description:   "Optional partial-index predicate (without the WHERE keyword).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
		},
	}
}

func (r *indexResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *indexResource) resolveDatabase(m indexModel) string {
	if m.Database.IsNull() || m.Database.IsUnknown() || m.Database.ValueString() == "" {
		return r.client.DatabaseName()
	}
	return m.Database.ValueString()
}

// allowedMethods guards the (unquoted) USING clause against injection.
var allowedMethods = map[string]bool{
	"btree": true, "hash": true, "gin": true, "gist": true, "spgist": true, "brin": true,
}

func (r *indexResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data indexModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	database := r.resolveDatabase(data)
	schemaName := data.Schema.ValueString()
	if schemaName == "" {
		schemaName = "public"
	}
	method := data.Method.ValueString()
	if method == "" {
		method = "btree"
	}
	if !allowedMethods[strings.ToLower(method)] {
		resp.Diagnostics.AddError("invalid index method", fmt.Sprintf("%q is not a supported index method", method))
		return
	}

	var columns []string
	resp.Diagnostics.Append(data.Columns.ElementsAs(ctx, &columns, false)...)
	if resp.Diagnostics.HasError() {
		return
	}

	db, err := r.client.ForDatabase(database).Connect()
	if err != nil {
		resp.Diagnostics.AddError("could not connect to database", err.Error())
		return
	}

	quoted := make([]string, len(columns))
	for i, c := range columns {
		quoted[i] = pq.QuoteIdentifier(c)
	}

	var b strings.Builder
	b.WriteString("CREATE ")
	if data.Unique.ValueBool() {
		b.WriteString("UNIQUE ")
	}
	fmt.Fprintf(&b, "INDEX %s ON %s.%s USING %s (%s)",
		pq.QuoteIdentifier(data.Name.ValueString()),
		pq.QuoteIdentifier(schemaName),
		pq.QuoteIdentifier(data.Table.ValueString()),
		strings.ToLower(method),
		strings.Join(quoted, ", "),
	)
	if w := data.Where.ValueString(); w != "" {
		b.WriteString(" WHERE " + w)
	}

	if _, err := db.Exec(b.String()); err != nil {
		resp.Diagnostics.AddError("could not create index", err.Error())
		return
	}

	data.Database = types.StringValue(database)
	data.Schema = types.StringValue(schemaName)
	data.Method = types.StringValue(strings.ToLower(method))
	data.ID = types.StringValue(fmt.Sprintf("%s.%s.%s", database, schemaName, data.Name.ValueString()))
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *indexResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data indexModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	database := r.resolveDatabase(data)
	schemaName := data.Schema.ValueString()
	if schemaName == "" {
		schemaName = "public"
	}

	db, err := r.client.ForDatabase(database).Connect()
	if err != nil {
		resp.Diagnostics.AddError("could not connect to database", err.Error())
		return
	}

	var tablename, indexdef string
	err = db.QueryRow(
		"SELECT tablename, indexdef FROM pg_indexes WHERE schemaname = $1 AND indexname = $2",
		schemaName, data.Name.ValueString(),
	).Scan(&tablename, &indexdef)
	if errors.Is(err, sql.ErrNoRows) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("could not read index", err.Error())
		return
	}

	data.Database = types.StringValue(database)
	data.Schema = types.StringValue(schemaName)
	data.Table = types.StringValue(tablename)
	data.ID = types.StringValue(fmt.Sprintf("%s.%s.%s", database, schemaName, data.Name.ValueString()))
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// Update only runs when a non-replacing attribute changes; all configurable
// attributes force replacement, so this just persists the planned values.
func (r *indexResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data indexModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *indexResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data indexModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	database := r.resolveDatabase(data)
	db, err := r.client.ForDatabase(database).Connect()
	if err != nil {
		resp.Diagnostics.AddError("could not connect to database", err.Error())
		return
	}

	stmt := fmt.Sprintf("DROP INDEX IF EXISTS %s.%s",
		pq.QuoteIdentifier(data.Schema.ValueString()),
		pq.QuoteIdentifier(data.Name.ValueString()),
	)
	if _, err := db.Exec(stmt); err != nil {
		resp.Diagnostics.AddError("could not drop index", err.Error())
	}
}

// ImportState accepts "database.schema.name". Table is then filled in by Read;
// columns/options are left for the next plan to reconcile.
func (r *indexResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, ".", 3)
	if len(parts) != 3 {
		resp.Diagnostics.AddError("invalid import ID", "expected format \"database.schema.name\"")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("database"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("schema"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), parts[2])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
