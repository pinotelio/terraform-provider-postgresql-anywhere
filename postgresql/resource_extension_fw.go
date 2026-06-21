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
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/lib/pq"
)

var (
	_ resource.Resource                = (*extensionResource)(nil)
	_ resource.ResourceWithConfigure   = (*extensionResource)(nil)
	_ resource.ResourceWithImportState = (*extensionResource)(nil)
)

// NewExtensionResource returns the postgresql_extension resource.
func NewExtensionResource() resource.Resource {
	return &extensionResource{}
}

type extensionResource struct {
	client *Client
}

type extensionModel struct {
	ID            types.String `tfsdk:"id"`
	Name          types.String `tfsdk:"name"`
	Schema        types.String `tfsdk:"schema"`
	Version       types.String `tfsdk:"version"`
	Database      types.String `tfsdk:"database"`
	DropCascade   types.Bool   `tfsdk:"drop_cascade"`
	CreateCascade types.Bool   `tfsdk:"create_cascade"`
}

func (r *extensionResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_extension"
}

func (r *extensionResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a PostgreSQL extension on a database.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Synthetic id: database.name",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:      true,
				Description:   "Name of the extension.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"schema": schema.StringAttribute{
				Optional:      true,
				Computed:      true,
				Description:   "Sets the schema of an extension",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"version": schema.StringAttribute{
				Optional:      true,
				Computed:      true,
				Description:   "Sets the version number of the extension",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"database": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Sets the database to add the extension to",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"drop_cascade": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "When true, will also drop all the objects that depend on the extension, and in turn all objects that depend on those objects",
			},
			"create_cascade": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "When true, will also create any extensions that this extension depends on that are not already installed",
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *extensionResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *extensionResource) resolveDatabase(m extensionModel) string {
	if m.Database.IsNull() || m.Database.IsUnknown() || m.Database.ValueString() == "" {
		return r.client.DatabaseName()
	}
	return m.Database.ValueString()
}

// extensionID builds the synthetic id "database.name".
func extensionID(database, name string) string {
	return strings.Join([]string{database, name}, ".")
}

// readExtension reads the extension's schema and version from the given
// database. found is false when the extension does not exist.
func (r *extensionResource) readExtension(database, extName string) (found bool, extSchema, extVersion string, err error) {
	txn, err := startTransaction(r.client, database)
	if err != nil {
		return false, "", "", err
	}
	defer deferredRollback(txn)

	query := `SELECT n.nspname, e.extversion ` +
		`FROM pg_catalog.pg_extension e, pg_catalog.pg_namespace n ` +
		`WHERE n.oid = e.extnamespace AND e.extname = $1`
	err = txn.QueryRow(query, extName).Scan(&extSchema, &extVersion)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, "", "", nil
	case err != nil:
		return false, "", "", fmt.Errorf("error reading extension: %w", err)
	}
	return true, extSchema, extVersion, nil
}

func (r *extensionResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data extensionModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	database := r.resolveDatabase(data)

	db, err := r.client.ForDatabase(database).Connect()
	if err != nil {
		resp.Diagnostics.AddError("could not connect to database", err.Error())
		return
	}
	if !db.featureSupported(featureExtension) {
		resp.Diagnostics.AddError(
			"extension not supported",
			fmt.Sprintf("postgresql_extension resource is not supported for this Postgres version (%s)", db.version),
		)
		return
	}

	extName := data.Name.ValueString()

	b := bytes.NewBufferString("CREATE EXTENSION IF NOT EXISTS ")
	fmt.Fprint(b, pq.QuoteIdentifier(extName))

	if v := data.Schema; !v.IsNull() && !v.IsUnknown() && v.ValueString() != "" {
		fmt.Fprint(b, " SCHEMA ", pq.QuoteIdentifier(v.ValueString()))
	}

	if v := data.Version; !v.IsNull() && !v.IsUnknown() && v.ValueString() != "" {
		fmt.Fprint(b, " VERSION ", pq.QuoteIdentifier(v.ValueString()))
	}

	if data.CreateCascade.ValueBool() {
		fmt.Fprint(b, " CASCADE")
	}

	txn, err := startTransaction(r.client, database)
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	if _, err := txn.Exec(b.String()); err != nil {
		resp.Diagnostics.AddError("could not create extension", err.Error())
		return
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("error creating extension", err.Error())
		return
	}

	found, extSchema, extVersion, err := r.readExtension(database, extName)
	if err != nil {
		resp.Diagnostics.AddError("could not read extension", err.Error())
		return
	}
	if !found {
		resp.Diagnostics.AddError(
			"extension not found after create",
			fmt.Sprintf("extension %q was not found in database %q after creation", extName, database),
		)
		return
	}

	data.Name = types.StringValue(extName)
	data.Schema = types.StringValue(extSchema)
	data.Version = types.StringValue(extVersion)
	data.Database = types.StringValue(database)
	data.ID = types.StringValue(extensionID(database, extName))
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *extensionResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data extensionModel
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
	if !db.featureSupported(featureExtension) {
		resp.Diagnostics.AddError(
			"extension not supported",
			fmt.Sprintf("postgresql_extension resource is not supported for this Postgres version (%s)", db.version),
		)
		return
	}

	extName := data.Name.ValueString()

	found, extSchema, extVersion, err := r.readExtension(database, extName)
	if err != nil {
		resp.Diagnostics.AddError("could not read extension", err.Error())
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}

	if data.DropCascade.IsNull() || data.DropCascade.IsUnknown() {
		data.DropCascade = types.BoolValue(false)
	}
	if data.CreateCascade.IsNull() || data.CreateCascade.IsUnknown() {
		data.CreateCascade = types.BoolValue(false)
	}

	data.Name = types.StringValue(extName)
	data.Schema = types.StringValue(extSchema)
	data.Version = types.StringValue(extVersion)
	data.Database = types.StringValue(database)
	data.ID = types.StringValue(extensionID(database, extName))
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *extensionResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state extensionModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	database := r.resolveDatabase(plan)

	db, err := r.client.ForDatabase(database).Connect()
	if err != nil {
		resp.Diagnostics.AddError("could not connect to database", err.Error())
		return
	}
	if !db.featureSupported(featureExtension) {
		resp.Diagnostics.AddError(
			"extension not supported",
			fmt.Sprintf("postgresql_extension resource is not supported for this Postgres version (%s)", db.version),
		)
		return
	}

	extName := plan.Name.ValueString()

	txn, err := startTransaction(r.client, database)
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	// Can't rename a schema

	if !plan.Schema.Equal(state.Schema) {
		n := plan.Schema.ValueString()
		if n == "" {
			resp.Diagnostics.AddError("error updating extension SCHEMA", "error setting extension name to an empty string")
			return
		}
		stmt := fmt.Sprintf("ALTER EXTENSION %s SET SCHEMA %s",
			pq.QuoteIdentifier(extName), pq.QuoteIdentifier(n))
		if _, err := txn.Exec(stmt); err != nil {
			resp.Diagnostics.AddError("error updating extension SCHEMA", err.Error())
			return
		}
	}

	if !plan.Version.Equal(state.Version) {
		b := bytes.NewBufferString("ALTER EXTENSION ")
		fmt.Fprintf(b, "%s UPDATE", pq.QuoteIdentifier(extName))

		n := plan.Version.ValueString()
		if n != "" {
			fmt.Fprintf(b, " TO %s", pq.QuoteIdentifier(n))
		}
		if _, err := txn.Exec(b.String()); err != nil {
			resp.Diagnostics.AddError("error updating extension version", err.Error())
			return
		}
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("error updating extension", err.Error())
		return
	}

	found, extSchema, extVersion, err := r.readExtension(database, extName)
	if err != nil {
		resp.Diagnostics.AddError("could not read extension", err.Error())
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}

	plan.Name = types.StringValue(extName)
	plan.Schema = types.StringValue(extSchema)
	plan.Version = types.StringValue(extVersion)
	plan.Database = types.StringValue(database)
	plan.ID = types.StringValue(extensionID(database, extName))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *extensionResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data extensionModel
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
	if !db.featureSupported(featureExtension) {
		resp.Diagnostics.AddError(
			"extension not supported",
			fmt.Sprintf("postgresql_extension resource is not supported for this Postgres version (%s)", db.version),
		)
		return
	}

	extName := data.Name.ValueString()

	txn, err := startTransaction(r.client, database)
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	dropMode := "RESTRICT"
	if data.DropCascade.ValueBool() {
		dropMode = "CASCADE"
	}

	stmt := fmt.Sprintf("DROP EXTENSION %s %s ", pq.QuoteIdentifier(extName), dropMode)
	if _, err := txn.Exec(stmt); err != nil {
		resp.Diagnostics.AddError("could not drop extension", err.Error())
		return
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("error deleting extension", err.Error())
	}
}

// ImportState accepts "database.name".
func (r *extensionResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parsed := strings.Split(req.ID, ".")
	if len(parsed) != 2 {
		resp.Diagnostics.AddError(
			"invalid import ID",
			fmt.Sprintf("extension ID %s has not the expected format 'database.extension': %v", req.ID, parsed),
		)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("database"), parsed[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), parsed[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
