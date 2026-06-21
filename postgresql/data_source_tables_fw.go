package postgresql

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*tablesDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*tablesDataSource)(nil)
)

// tablesObjectType is the element type of the computed "tables" list.
var tablesObjectType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"object_name": types.StringType,
	"schema_name": types.StringType,
	"table_type":  types.StringType,
}}

// Catalog query and column keywords for the tables data source.
const (
	tableQuery = `
	SELECT table_name, table_schema, table_type
	FROM information_schema.tables
	`
	tablePatternMatchingTarget = "table_name"
	tableSchemaKeyword         = "table_schema"
	tableTypeKeyword           = "table_type"
)

// NewTablesDataSource returns the postgresql_tables data source.
func NewTablesDataSource() datasource.DataSource {
	return &tablesDataSource{}
}

type tablesDataSource struct {
	client *Client
}

type tablesDataSourceModel struct {
	ID                 types.String `tfsdk:"id"`
	Database           types.String `tfsdk:"database"`
	Schemas            types.List   `tfsdk:"schemas"`
	TableTypes         types.List   `tfsdk:"table_types"`
	LikeAnyPatterns    types.List   `tfsdk:"like_any_patterns"`
	LikeAllPatterns    types.List   `tfsdk:"like_all_patterns"`
	NotLikeAllPatterns types.List   `tfsdk:"not_like_all_patterns"`
	RegexPattern       types.String `tfsdk:"regex_pattern"`
	Tables             types.List   `tfsdk:"tables"`
}

type tablesTableModel struct {
	ObjectName types.String `tfsdk:"object_name"`
	SchemaName types.String `tfsdk:"schema_name"`
	TableType  types.String `tfsdk:"table_type"`
}

func (d *tablesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_tables"
}

func (d *tablesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Retrieves a list of table names from a PostgreSQL database.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Synthetic id derived from the database name and the supplied filters.",
			},
			"database": schema.StringAttribute{
				Required:    true,
				Description: "The PostgreSQL database which will be queried for table names",
			},
			"schemas": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "The PostgreSQL schema(s) which will be queried for table names. Queries all schemas in the database by default",
			},
			"table_types": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "The PostgreSQL table types which will be queried for table names. Includes all table types by default. Use 'BASE TABLE' for normal tables only",
			},
			"like_any_patterns": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "Expression(s) which will be pattern matched against table names in the query using the PostgreSQL LIKE ANY operator",
			},
			"like_all_patterns": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "Expression(s) which will be pattern matched against table names in the query using the PostgreSQL LIKE ALL operator",
			},
			"not_like_all_patterns": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "Expression(s) which will be pattern matched against table names in the query using the PostgreSQL NOT LIKE ALL operator",
			},
			"regex_pattern": schema.StringAttribute{
				Optional:    true,
				Description: "Expression which will be pattern matched against table names in the query using the PostgreSQL ~ (regular expression match) operator",
			},
			"tables": schema.ListNestedAttribute{
				Computed:    true,
				Description: "The list of PostgreSQL tables retrieved by this data source. Note that this returns a set, so duplicate table names across different schemas will be consolidated.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"object_name": schema.StringAttribute{
							Computed: true,
						},
						"schema_name": schema.StringAttribute{
							Computed: true,
						},
						"table_type": schema.StringAttribute{
							Computed: true,
						},
					},
				},
			},
		},
	}
}

func (d *tablesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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
	d.client = client
}

func (d *tablesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data tablesDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	database := data.Database.ValueString()

	txn, err := startTransaction(d.client, database)
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	var schemas, tableTypes, likeAny, likeAll, notLikeAll []string
	resp.Diagnostics.Append(data.Schemas.ElementsAs(ctx, &schemas, false)...)
	resp.Diagnostics.Append(data.TableTypes.ElementsAs(ctx, &tableTypes, false)...)
	resp.Diagnostics.Append(data.LikeAnyPatterns.ElementsAs(ctx, &likeAny, false)...)
	resp.Diagnostics.Append(data.LikeAllPatterns.ElementsAs(ctx, &likeAll, false)...)
	resp.Diagnostics.Append(data.NotLikeAllPatterns.ElementsAs(ctx, &notLikeAll, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	regexPattern := data.RegexPattern.ValueString()

	query := tableQuery
	queryConcatKeyword := queryConcatKeywordWhere

	filters := []string{}
	schemasTypeFilter := applyTypeMatchingToQuery(tableSchemaKeyword, fwStringsToAny(schemas))
	if len(schemasTypeFilter) > 0 {
		filters = append(filters, schemasTypeFilter)
	}
	tableTypeFilter := applyTypeMatchingToQuery(tableTypeKeyword, fwStringsToAny(tableTypes))
	if len(tableTypeFilter) > 0 {
		filters = append(filters, tableTypeFilter)
	}
	filters = append(filters, dataSourcePatternFiltersFW(tablePatternMatchingTarget, likeAny, likeAll, notLikeAll, regexPattern)...)
	query = finalizeQueryWithFilters(query, queryConcatKeyword, filters)

	rows, err := txn.Query(query)
	if err != nil {
		resp.Diagnostics.AddError("could not query tables", err.Error())
		return
	}
	defer func() {
		if err := rows.Close(); err != nil {
			log.Printf("error closing rows: %v\n", err)
		}
	}()

	tables := make([]tablesTableModel, 0)
	for rows.Next() {
		var objectName, schemaName, tableType string
		if err = rows.Scan(&objectName, &schemaName, &tableType); err != nil {
			resp.Diagnostics.AddError("could not scan table output for database", err.Error())
			return
		}
		tables = append(tables, tablesTableModel{
			ObjectName: types.StringValue(objectName),
			SchemaName: types.StringValue(schemaName),
			TableType:  types.StringValue(tableType),
		})
	}

	tablesList, diags := types.ListValueFrom(ctx, tablesObjectType, tables)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	data.Tables = tablesList
	data.ID = types.StringValue(strings.Join([]string{
		database,
		generatePatternArrayString(fwStringsToAny(schemas), queryArrayKeywordAny),
		generatePatternArrayString(fwStringsToAny(tableTypes), queryArrayKeywordAny),
		generatePatternArrayString(fwStringsToAny(likeAny), queryArrayKeywordAny),
		generatePatternArrayString(fwStringsToAny(likeAll), queryArrayKeywordAll),
		generatePatternArrayString(fwStringsToAny(notLikeAll), queryArrayKeywordAll),
		regexPattern,
	}, "_"))

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
