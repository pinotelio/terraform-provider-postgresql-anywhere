package postgresql

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*schemasDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*schemasDataSource)(nil)
)

// schemaQueries holds the catalog query for each system-schema inclusion mode.
var schemaQueries = map[string]string{
	"query_include_system_schemas": `
	SELECT schema_name
	FROM information_schema.schemata
	`,
	"query_exclude_system_schemas": `
	SELECT schema_name
	FROM information_schema.schemata
	WHERE schema_name NOT LIKE 'pg_%'
	AND schema_name <> 'information_schema'
	`,
}

const schemaPatternMatchingTarget = "schema_name"

// NewSchemasDataSource returns the postgresql_schemas data source.
func NewSchemasDataSource() datasource.DataSource {
	return &schemasDataSource{}
}

type schemasDataSource struct {
	client *Client
}

type schemasDataSourceModel struct {
	ID                   types.String `tfsdk:"id"`
	Database             types.String `tfsdk:"database"`
	IncludeSystemSchemas types.Bool   `tfsdk:"include_system_schemas"`
	LikeAnyPatterns      types.List   `tfsdk:"like_any_patterns"`
	LikeAllPatterns      types.List   `tfsdk:"like_all_patterns"`
	NotLikeAllPatterns   types.List   `tfsdk:"not_like_all_patterns"`
	RegexPattern         types.String `tfsdk:"regex_pattern"`
	Schemas              types.Set    `tfsdk:"schemas"`
}

// fwStringsToAny converts a slice of strings to a slice of any so it can be fed
// to the query helpers, which operate on []any.
func fwStringsToAny(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// dataSourcePatternFiltersFW builds the LIKE/regex SQL filters for a query from
// the configured pattern lists.
func dataSourcePatternFiltersFW(patternMatchingTarget string, likeAny, likeAll, notLikeAll []string, regexPattern string) []string {
	filters := []string{}
	if len(likeAny) > 0 {
		filters = append(filters, generatePatternMatchingString(patternMatchingTarget, likePatternQuery, generatePatternArrayString(fwStringsToAny(likeAny), queryArrayKeywordAny)))
	}
	if len(likeAll) > 0 {
		filters = append(filters, generatePatternMatchingString(patternMatchingTarget, likePatternQuery, generatePatternArrayString(fwStringsToAny(likeAll), queryArrayKeywordAll)))
	}
	if len(notLikeAll) > 0 {
		filters = append(filters, generatePatternMatchingString(patternMatchingTarget, notLikePatternQuery, generatePatternArrayString(fwStringsToAny(notLikeAll), queryArrayKeywordAll)))
	}
	if regexPattern != "" {
		filters = append(filters, generatePatternMatchingString(patternMatchingTarget, regexPatternQuery, fmt.Sprintf("'%s'", regexPattern)))
	}
	return filters
}

func (d *schemasDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_schemas"
}

func (d *schemasDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Retrieves a list of schema names from a PostgreSQL database.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Synthetic id derived from the database name and the supplied filters.",
			},
			"database": schema.StringAttribute{
				Required:    true,
				Description: "The PostgreSQL database which will be queried for schema names",
			},
			"include_system_schemas": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Determines whether to include system schemas (pg_ prefix and information_schema). 'public' will always be included.",
			},
			"like_any_patterns": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "Expression(s) which will be pattern matched in the query using the PostgreSQL LIKE ANY operator",
			},
			"like_all_patterns": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "Expression(s) which will be pattern matched in the query using the PostgreSQL LIKE ALL operator",
			},
			"not_like_all_patterns": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "Expression(s) which will be pattern matched in the query using the PostgreSQL NOT LIKE ALL operator",
			},
			"regex_pattern": schema.StringAttribute{
				Optional:    true,
				Description: "Expression which will be pattern matched in the query using the PostgreSQL ~ (regular expression match) operator",
			},
			"schemas": schema.SetAttribute{
				ElementType: types.StringType,
				Computed:    true,
				Description: "The list of PostgreSQL schemas retrieved by this data source",
			},
		},
	}
}

func (d *schemasDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *schemasDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data schemasDataSourceModel
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

	includeSystemSchemas := data.IncludeSystemSchemas.ValueBool()

	var likeAny, likeAll, notLikeAll []string
	resp.Diagnostics.Append(data.LikeAnyPatterns.ElementsAs(ctx, &likeAny, false)...)
	resp.Diagnostics.Append(data.LikeAllPatterns.ElementsAs(ctx, &likeAll, false)...)
	resp.Diagnostics.Append(data.NotLikeAllPatterns.ElementsAs(ctx, &notLikeAll, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	regexPattern := data.RegexPattern.ValueString()

	var query string
	var queryConcatKeyword string
	if includeSystemSchemas {
		query = schemaQueries["query_include_system_schemas"]
		queryConcatKeyword = queryConcatKeywordWhere
	} else {
		query = schemaQueries["query_exclude_system_schemas"]
		queryConcatKeyword = queryConcatKeywordAnd
	}

	filters := dataSourcePatternFiltersFW(schemaPatternMatchingTarget, likeAny, likeAll, notLikeAll, regexPattern)
	query = finalizeQueryWithFilters(query, queryConcatKeyword, filters)

	rows, err := txn.Query(query)
	if err != nil {
		resp.Diagnostics.AddError("could not query schemas", err.Error())
		return
	}
	defer func() {
		if err := rows.Close(); err != nil {
			log.Printf("error closing rows: %v", err)
		}
	}()

	schemas := []string{}
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			resp.Diagnostics.AddError("could not scan schema name for database", err.Error())
			return
		}
		schemas = append(schemas, s)
	}

	schemasSet, diags := types.SetValueFrom(ctx, types.StringType, schemas)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	data.Schemas = schemasSet
	data.IncludeSystemSchemas = types.BoolValue(includeSystemSchemas)
	data.ID = types.StringValue(strings.Join([]string{
		database,
		strconv.FormatBool(includeSystemSchemas),
		generatePatternArrayString(fwStringsToAny(likeAny), queryArrayKeywordAny),
		generatePatternArrayString(fwStringsToAny(likeAll), queryArrayKeywordAll),
		generatePatternArrayString(fwStringsToAny(notLikeAll), queryArrayKeywordAll),
		regexPattern,
	}, "_"))

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
