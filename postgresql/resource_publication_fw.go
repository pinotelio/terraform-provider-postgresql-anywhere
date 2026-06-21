package postgresql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/lib/pq"
)

var (
	_ resource.Resource                = (*publicationResource)(nil)
	_ resource.ResourceWithConfigure   = (*publicationResource)(nil)
	_ resource.ResourceWithImportState = (*publicationResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*publicationResource)(nil)
)

// NewPublicationResource returns the postgresql_publication resource.
func NewPublicationResource() resource.Resource {
	return &publicationResource{}
}

type publicationResource struct {
	client *Client
}

type publicationResourceModel struct {
	ID             types.String `tfsdk:"id"`
	Name           types.String `tfsdk:"name"`
	Database       types.String `tfsdk:"database"`
	Owner          types.String `tfsdk:"owner"`
	Tables         types.Set    `tfsdk:"tables"`
	AllTables      types.Bool   `tfsdk:"all_tables"`
	PublishParam   types.List   `tfsdk:"publish_param"`
	PublishViaRoot types.Bool   `tfsdk:"publish_via_partition_root_param"`
	DropCascade    types.Bool   `tfsdk:"drop_cascade"`
}

func (r *publicationResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_publication"
}

func (r *publicationResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a PostgreSQL publication.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Synthetic id: database.name",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "The name of the publication",
				Validators:  []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"database": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Sets the database to add the publication for",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"owner": schema.StringAttribute{
				Optional:      true,
				Computed:      true,
				Description:   "Sets the owner of the publication",
				Validators:    []validator.String{stringvalidator.LengthAtLeast(1)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"tables": schema.SetAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Computed:    true,
				Description: "Sets the tables list to publish",
				Validators: []validator.Set{
					setvalidator.ConflictsWith(path.MatchRoot("all_tables")),
				},
				PlanModifiers: []planmodifier.Set{setplanmodifier.UseStateForUnknown()},
			},
			"all_tables": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Sets the tables list to publish to ALL tables",
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.RequiresReplace(),
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"publish_param": schema.ListAttribute{
				ElementType:   types.StringType,
				Optional:      true,
				Computed:      true,
				Description:   "Sets which DML operations will be published",
				Validators:    []validator.List{listvalidator.SizeAtLeast(1)},
				PlanModifiers: []planmodifier.List{listplanmodifier.UseStateForUnknown()},
			},
			"publish_via_partition_root_param": schema.BoolAttribute{
				Optional:      true,
				Computed:      true,
				Description:   "Sets whether changes in a partitioned table using the identity and schema of the partitioned table",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"drop_cascade": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "When true, will also drop all the objects that depend on the publication, and in turn all objects that depend on those objects",
			},
		},
	}
}

func (r *publicationResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// ModifyPlan keeps the computed id (database.name) in sync with the name and
// database attributes. name is updatable in place (ALTER PUBLICATION ... RENAME),
// and the id embeds the name, so on a rename the planned id must change too —
// otherwise the id plan modifier (UseStateForUnknown) would keep the old value
// while apply writes the new one, yielding a "provider produced inconsistent
// result" error.
func (r *publicationResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		// Resource is being destroyed.
		return
	}
	var name, database types.String
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("name"), &name)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("database"), &database)...)
	if resp.Diagnostics.HasError() || name.IsUnknown() || name.IsNull() {
		return
	}

	db := ""
	if !database.IsUnknown() && !database.IsNull() && database.ValueString() != "" {
		db = database.ValueString()
	} else if r.client != nil {
		db = r.client.DatabaseName()
	} else {
		return
	}

	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("id"), types.StringValue(publicationID(db, name.ValueString())))...)
}

// resolveDatabase returns the database attribute if set (non-empty), otherwise
// the provider's default database.
func (r *publicationResource) resolveDatabase(m publicationResourceModel) string {
	if m.Database.IsNull() || m.Database.IsUnknown() || m.Database.ValueString() == "" {
		return r.client.DatabaseName()
	}
	return m.Database.ValueString()
}

// publicationID builds the synthetic id "database.name".
func publicationID(database, name string) string {
	return strings.Join([]string{database, name}, ".")
}

// connectAndCheck opens a connection to the target database and verifies the
// publication feature is supported.
func (r *publicationResource) connectAndCheck(database string, diags *diag.Diagnostics) *DBConnection {
	db, err := r.client.ForDatabase(database).Connect()
	if err != nil {
		diags.AddError("could not connect to database", err.Error())
		return nil
	}
	if !db.featureSupported(featurePublication) {
		diags.AddError(
			"feature not supported",
			fmt.Sprintf("postgresql_publication resource is not supported for this Postgres version (%s)", db.version),
		)
		return nil
	}
	return db
}

// validatePublicationPublishParamsFW rejects duplicate and unknown publish
// parameters. The duplicate error names the `tables` attribute (rather than
// `publish_param`) to match the message Postgres users already expect.
func validatePublicationPublishParamsFW(params []string) ([]string, error) {
	seen := make(map[string]bool, len(params))
	for _, p := range params {
		if seen[p] {
			return nil, fmt.Errorf("'%s' is duplicated for attribute `tables`", p)
		}
		seen[p] = true
	}

	valid := []string{"insert", "update", "delete", "truncate"}
	out := make([]string, 0, len(params))
	for _, attr := range params {
		if !sliceContainsStr(valid, attr) {
			return nil, fmt.Errorf("invalid value of `publish_param`: %s. Should be at least one of '%s'", attr, strings.Join(valid, ", "))
		}
		out = append(out, attr)
	}
	return out, nil
}

// buildPublicationParamClause renders the parameter map into the given clause
// template, or returns "" when there are no parameters.
func buildPublicationParamClause(pubParams map[string]string, template string) string {
	if len(pubParams) == 0 {
		return ""
	}
	var list []string
	for k, v := range pubParams {
		list = append(list, fmt.Sprintf("%s = %s", k, v))
	}
	return fmt.Sprintf(template, strings.Join(list, ","))
}

// stringSliceDifference returns the elements of a that are not present in b.
func stringSliceDifference(a, b []string) []string {
	m := make(map[string]bool, len(b))
	for _, x := range b {
		m[x] = true
	}
	var diff []string
	for _, x := range a {
		if !m[x] {
			diff = append(diff, x)
		}
	}
	return diff
}

// tablesClause builds the FOR [ALL] TABLE(S) clause used at creation time.
func (r *publicationResource) tablesClause(ctx context.Context, config publicationResourceModel, diags *diag.Diagnostics) string {
	allTables := !config.AllTables.IsNull() && !config.AllTables.IsUnknown() && config.AllTables.ValueBool()

	var tables []string
	if !config.Tables.IsNull() && !config.Tables.IsUnknown() {
		diags.Append(config.Tables.ElementsAs(ctx, &tables, false)...)
		if diags.HasError() {
			return ""
		}
	}

	clause := ""
	if allTables {
		clause = "FOR ALL TABLES"
	}
	if len(tables) > 0 {
		quoted := make([]string, 0, len(tables))
		for _, t := range tables {
			quoted = append(quoted, quoteTableName(t))
		}
		clause = fmt.Sprintf("FOR TABLE %s", strings.Join(quoted, ", "))
	}
	return clause
}

// createParams builds the WITH (...) parameter clause for a new publication.
func (r *publicationResource) createParams(ctx context.Context, config publicationResourceModel, pubViaRootEnabled bool) (string, error) {
	pubParams := make(map[string]string, 2)

	if !config.PublishViaRoot.IsNull() && !config.PublishViaRoot.IsUnknown() && config.PublishViaRoot.ValueBool() {
		if !pubViaRootEnabled {
			return "", fmt.Errorf("publish_via_partition_root attribute is supported only for postgres version 13 and above")
		}
		pubParams["publish_via_partition_root"] = fmt.Sprintf("%v", true)
	}

	if !config.PublishParam.IsNull() && !config.PublishParam.IsUnknown() {
		var params []string
		if d := config.PublishParam.ElementsAs(ctx, &params, false); d.HasError() {
			return "", fmt.Errorf("could not read publish_param")
		}
		if len(params) > 0 {
			paramsList, err := validatePublicationPublishParamsFW(params)
			if err != nil {
				return "", err
			}
			pubParams["publish"] = fmt.Sprintf("'%s'", strings.Join(paramsList, ", "))
		}
	}

	return buildPublicationParamClause(pubParams, "WITH (%s)"), nil
}

// updateParams builds the SET (...) parameter clause for an existing
// publication, emitting only the parameters that changed.
func (r *publicationResource) updateParams(ctx context.Context, state, plan publicationResourceModel, pubViaRootEnabled bool) (string, error) {
	pubParams := make(map[string]string, 2)

	if !plan.PublishViaRoot.Equal(state.PublishViaRoot) {
		if !pubViaRootEnabled {
			return "", fmt.Errorf("publish_via_partition_root attribute is supported only for postgres version 13 and above")
		}
		pubParams["publish_via_partition_root"] = fmt.Sprintf("%v", plan.PublishViaRoot.ValueBool())
	}

	if !plan.PublishParam.Equal(state.PublishParam) {
		var params []string
		if d := plan.PublishParam.ElementsAs(ctx, &params, false); d.HasError() {
			return "", fmt.Errorf("could not read publish_param")
		}
		paramsList, err := validatePublicationPublishParamsFW(params)
		if err != nil {
			return "", err
		}
		pubParams["publish"] = fmt.Sprintf("'%s'", strings.Join(paramsList, ", "))
	}

	return buildPublicationParamClause(pubParams, "SET (%s)"), nil
}

func (r *publicationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan, config publicationResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	database := r.resolveDatabase(plan)
	db := r.connectAndCheck(database, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	name := plan.Name.ValueString()

	tablesClause := r.tablesClause(ctx, config, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	paramsClause, err := r.createParams(ctx, config, db.featureSupported(featurePublishViaRoot))
	if err != nil {
		resp.Diagnostics.AddError("could not get publication parameters", err.Error())
		return
	}

	txn, err := startTransaction(r.client, database)
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	createSQL := fmt.Sprintf("CREATE PUBLICATION %s %s %s", name, tablesClause, paramsClause)
	if _, err := txn.Exec(createSQL); err != nil {
		resp.Diagnostics.AddError("error creating Publication", err.Error())
		return
	}

	// Set the owner if it was explicitly provided.
	if !config.Owner.IsNull() && !config.Owner.IsUnknown() && config.Owner.ValueString() != "" {
		ownerSQL := fmt.Sprintf("ALTER PUBLICATION %s OWNER TO %s", name, config.Owner.ValueString())
		if _, err := txn.Exec(ownerSQL); err != nil {
			resp.Diagnostics.AddError("could not set publication owner during creation", err.Error())
			return
		}
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("error creating Publication", err.Error())
		return
	}

	plan.Database = types.StringValue(database)
	plan.ID = types.StringValue(publicationID(database, name))

	found := r.read(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.Diagnostics.AddError("could not read publication", "publication was not found right after creation")
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *publicationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data publicationResourceModel
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
// publication no longer exists.
func (r *publicationResource) read(ctx context.Context, data *publicationResourceModel, diags *diag.Diagnostics) bool {
	database := r.resolveDatabase(*data)
	pubName := data.Name.ValueString()

	db := r.connectAndCheck(database, diags)
	if diags.HasError() {
		return false
	}

	txn, err := startTransaction(r.client, database)
	if err != nil {
		diags.AddError("could not start transaction", err.Error())
		return false
	}
	defer deferredRollback(txn)

	var tables []string
	var publishParams []string
	var puballtables, pubinsert, pubupdate, pubdelete, pubtruncate, pubviaroot bool
	var pubowner string
	columns := []string{"puballtables", "pubinsert", "pubupdate", "pubdelete", "r.rolname as pubownername"}
	values := []any{
		&puballtables,
		&pubinsert,
		&pubupdate,
		&pubdelete,
		&pubowner,
	}

	viaRootSupported := db.featureSupported(featurePublishViaRoot)
	if viaRootSupported {
		columns = append(columns, "pubviaroot")
		values = append(values, &pubviaroot)
	}
	if db.featureSupported(featurePubTruncate) {
		columns = append(columns, "pubtruncate")
		values = append(values, &pubtruncate)
	}

	query := fmt.Sprintf("SELECT %s FROM pg_catalog.pg_publication as p join pg_catalog.pg_roles as r on p.pubowner = r.oid WHERE pubname = $1", strings.Join(columns, ", "))
	err = txn.QueryRow(query, pqQuoteLiteral(pubName)).Scan(values...)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		log.Printf("[WARN] PostgreSQL Publication (%s) not found for database %s", pubName, database)
		return false
	case err != nil:
		diags.AddError("error reading publication info", err.Error())
		return false
	}

	tablesQuery := `SELECT CONCAT(schemaname,'.',tablename) as fulltablename ` +
		`FROM pg_catalog.pg_publication_tables ` +
		`WHERE pubname = $1`

	rows, err := txn.Query(tablesQuery, pqQuoteLiteral(pubName))
	if err != nil {
		diags.AddError("could not get publication tables", err.Error())
		return false
	}
	defer func() {
		if err := rows.Close(); err != nil {
			log.Printf("error closing rows: %v", err)
		}
	}()

	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			diags.AddError("could not get tables", err.Error())
			return false
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		diags.AddError("got rows.Err", err.Error())
		return false
	}

	if pubinsert {
		publishParams = append(publishParams, "insert")
	}
	if pubupdate {
		publishParams = append(publishParams, "update")
	}
	if pubdelete {
		publishParams = append(publishParams, "delete")
	}
	if pubtruncate {
		publishParams = append(publishParams, "truncate")
	}

	data.Name = types.StringValue(pubName)
	data.Database = types.StringValue(database)
	data.Owner = types.StringValue(pubowner)

	tablesSet, d := types.SetValueFrom(ctx, types.StringType, tables)
	diags.Append(d...)
	if diags.HasError() {
		return false
	}
	data.Tables = tablesSet

	data.AllTables = types.BoolValue(puballtables)

	publishList, d2 := types.ListValueFrom(ctx, types.StringType, publishParams)
	diags.Append(d2...)
	if diags.HasError() {
		return false
	}
	data.PublishParam = publishList

	if viaRootSupported {
		data.PublishViaRoot = types.BoolValue(pubviaroot)
	} else {
		data.PublishViaRoot = types.BoolValue(false)
	}

	data.ID = types.StringValue(publicationID(database, pubName))
	return true
}

func (r *publicationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state publicationResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	database := r.resolveDatabase(plan)
	db := r.connectAndCheck(database, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	// Validate & build the publication parameters clause FIRST, before opening a
	// transaction or making any other changes, so an invalid publish_param
	// (unknown value or duplicate) surfaces a clean validation error rather than
	// being entangled with owner/table/name changes.
	paramsClause, err := r.updateParams(ctx, state, plan, db.featureSupported(featurePublishViaRoot))
	if err != nil {
		resp.Diagnostics.AddError("could not update publication parameters", err.Error())
		return
	}

	txn, err := startTransaction(r.client, database)
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	pubName := plan.Name.ValueString()

	// Owner (uses the unquoted name).
	if !plan.Owner.Equal(state.Owner) {
		ownerSQL := fmt.Sprintf("ALTER PUBLICATION %s OWNER TO %s", pubName, plan.Owner.ValueString())
		if _, err := txn.Exec(ownerSQL); err != nil {
			resp.Diagnostics.AddError("could not update publication owner", err.Error())
			return
		}
	}

	// Tables: diff added/dropped tables.
	if !plan.Tables.Equal(state.Tables) {
		if err := r.alterTables(ctx, txn, pubName, state, plan); err != nil {
			resp.Diagnostics.AddError("could not update publication tables", err.Error())
			return
		}
	}

	// Publication parameters. The clause was already validated/built above;
	// execute it here after the table changes.
	if paramsClause != "" {
		paramSQL := fmt.Sprintf("ALTER PUBLICATION %s %s", pubName, paramsClause)
		if _, err := txn.Exec(paramSQL); err != nil {
			resp.Diagnostics.AddError("error updating publication parameters", err.Error())
			return
		}
	}

	// Name: ALTER PUBLICATION ... RENAME TO ...
	if !plan.Name.Equal(state.Name) {
		renameSQL := fmt.Sprintf("ALTER PUBLICATION %s RENAME TO %s",
			pq.QuoteIdentifier(state.Name.ValueString()),
			pq.QuoteIdentifier(plan.Name.ValueString()),
		)
		if _, err := txn.Exec(renameSQL); err != nil {
			resp.Diagnostics.AddError("could not update publication name", err.Error())
			return
		}
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("error updating publication", err.Error())
		return
	}

	found := r.read(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// alterTables ADDs and DROPs tables to match the plan.
func (r *publicationResource) alterTables(ctx context.Context, txn *sql.Tx, pubName string, state, plan publicationResourceModel) error {
	var oldList, newList []string
	if !state.Tables.IsNull() && !state.Tables.IsUnknown() {
		_ = state.Tables.ElementsAs(ctx, &oldList, false)
	}
	if !plan.Tables.IsNull() && !plan.Tables.IsUnknown() {
		_ = plan.Tables.ElementsAs(ctx, &newList, false)
	}

	seen := make(map[string]bool, len(newList))
	for _, t := range newList {
		if seen[t] {
			return fmt.Errorf("'%s' is duplicated for attribute `tables`", t)
		}
		seen[t] = true
	}

	added := stringSliceDifference(newList, oldList)
	dropped := stringSliceDifference(oldList, newList)

	for _, t := range added {
		query := fmt.Sprintf("ALTER PUBLICATION %s ADD TABLE %s", pubName, quoteTableName(t))
		if _, err := txn.Exec(query); err != nil {
			return fmt.Errorf("could not alter publication table: %w", err)
		}
	}
	for _, t := range dropped {
		query := fmt.Sprintf("ALTER PUBLICATION %s DROP TABLE %s", pubName, quoteTableName(t))
		if _, err := txn.Exec(query); err != nil {
			return fmt.Errorf("could not alter publication table: %w", err)
		}
	}
	return nil
}

func (r *publicationResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data publicationResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	database := r.resolveDatabase(data)
	if r.connectAndCheck(database, &resp.Diagnostics); resp.Diagnostics.HasError() {
		return
	}

	publicationName := data.Name.ValueString()

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

	dropSQL := fmt.Sprintf("DROP PUBLICATION %s %s", pq.QuoteIdentifier(publicationName), dropMode)
	if _, err := txn.Exec(dropSQL); err != nil {
		resp.Diagnostics.AddError("could not execute sql", err.Error())
		return
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("error deleting Publication", err.Error())
	}
}

// ImportState accepts "database.name" (the publication ID format).
func (r *publicationResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(req.ID, ".")
	if len(parts) != 2 {
		resp.Diagnostics.AddError(
			"invalid import ID",
			fmt.Sprintf("publication ID %s has not the expected format 'database.publication_name'", req.ID),
		)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("database"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
