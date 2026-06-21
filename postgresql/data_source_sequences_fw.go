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
	_ datasource.DataSource              = (*sequencesDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*sequencesDataSource)(nil)
)

// sequencesObjectType is the element type of the computed "sequences" list.
var sequencesObjectType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"object_name": types.StringType,
	"schema_name": types.StringType,
	"data_type":   types.StringType,
}}

// Catalog query and column keywords for the sequences data source.
const (
	sequenceQuery = `
	SELECT sequence_name, sequence_schema, data_type
	FROM information_schema.sequences
	`
	sequencePatternMatchingTarget = "sequence_name"
	sequenceSchemaKeyword         = "sequence_schema"
)

// NewSequencesDataSource returns the postgresql_sequences data source.
func NewSequencesDataSource() datasource.DataSource {
	return &sequencesDataSource{}
}

type sequencesDataSource struct {
	client *Client
}

type sequencesDataSourceModel struct {
	ID                 types.String `tfsdk:"id"`
	Database           types.String `tfsdk:"database"`
	Schemas            types.List   `tfsdk:"schemas"`
	LikeAnyPatterns    types.List   `tfsdk:"like_any_patterns"`
	LikeAllPatterns    types.List   `tfsdk:"like_all_patterns"`
	NotLikeAllPatterns types.List   `tfsdk:"not_like_all_patterns"`
	RegexPattern       types.String `tfsdk:"regex_pattern"`
	Sequences          types.List   `tfsdk:"sequences"`
}

type sequencesSequenceModel struct {
	ObjectName types.String `tfsdk:"object_name"`
	SchemaName types.String `tfsdk:"schema_name"`
	DataType   types.String `tfsdk:"data_type"`
}

func (d *sequencesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_sequences"
}

func (d *sequencesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Retrieves a list of sequence names from a PostgreSQL database.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Synthetic id derived from the database name and the supplied filters.",
			},
			"database": schema.StringAttribute{
				Required:    true,
				Description: "The PostgreSQL database which will be queried for sequence names",
			},
			"schemas": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "The PostgreSQL schema(s) which will be queried for sequence names. Queries all schemas in the database by default",
			},
			"like_any_patterns": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "Expression(s) which will be pattern matched against sequence names in the query using the PostgreSQL LIKE ANY operator",
			},
			"like_all_patterns": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "Expression(s) which will be pattern matched against sequence names in the query using the PostgreSQL LIKE ALL operator",
			},
			"not_like_all_patterns": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "Expression(s) which will be pattern matched against sequence names in the query using the PostgreSQL NOT LIKE ALL operator",
			},
			"regex_pattern": schema.StringAttribute{
				Optional:    true,
				Description: "Expression which will be pattern matched against sequence names in the query using the PostgreSQL ~ (regular expression match) operator",
			},
			"sequences": schema.ListNestedAttribute{
				Computed:    true,
				Description: "The list of PostgreSQL sequence names retrieved by this data source. Note that this returns a set, so duplicate table names across different schemas will be consolidated.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"object_name": schema.StringAttribute{
							Computed: true,
						},
						"schema_name": schema.StringAttribute{
							Computed: true,
						},
						"data_type": schema.StringAttribute{
							Computed: true,
						},
					},
				},
			},
		},
	}
}

func (d *sequencesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *sequencesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data sequencesDataSourceModel
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

	var schemas, likeAny, likeAll, notLikeAll []string
	resp.Diagnostics.Append(data.Schemas.ElementsAs(ctx, &schemas, false)...)
	resp.Diagnostics.Append(data.LikeAnyPatterns.ElementsAs(ctx, &likeAny, false)...)
	resp.Diagnostics.Append(data.LikeAllPatterns.ElementsAs(ctx, &likeAll, false)...)
	resp.Diagnostics.Append(data.NotLikeAllPatterns.ElementsAs(ctx, &notLikeAll, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	regexPattern := data.RegexPattern.ValueString()

	query := sequenceQuery
	queryConcatKeyword := queryConcatKeywordWhere

	filters := []string{}
	schemasTypeFilter := applyTypeMatchingToQuery(sequenceSchemaKeyword, fwStringsToAny(schemas))
	if len(schemasTypeFilter) > 0 {
		filters = append(filters, schemasTypeFilter)
	}
	filters = append(filters, dataSourcePatternFiltersFW(sequencePatternMatchingTarget, likeAny, likeAll, notLikeAll, regexPattern)...)
	query = finalizeQueryWithFilters(query, queryConcatKeyword, filters)

	rows, err := txn.Query(query)
	if err != nil {
		resp.Diagnostics.AddError("could not query sequences", err.Error())
		return
	}
	defer func() {
		if err := rows.Close(); err != nil {
			log.Printf("error closing rows: %v", err)
		}
	}()

	sequences := make([]sequencesSequenceModel, 0)
	for rows.Next() {
		var objectName, schemaName, dataType string
		if err = rows.Scan(&objectName, &schemaName, &dataType); err != nil {
			resp.Diagnostics.AddError("could not scan sequence output for database", err.Error())
			return
		}
		sequences = append(sequences, sequencesSequenceModel{
			ObjectName: types.StringValue(objectName),
			SchemaName: types.StringValue(schemaName),
			DataType:   types.StringValue(dataType),
		})
	}

	sequencesList, diags := types.ListValueFrom(ctx, sequencesObjectType, sequences)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	data.Sequences = sequencesList
	data.ID = types.StringValue(strings.Join([]string{
		database,
		generatePatternArrayString(fwStringsToAny(schemas), queryArrayKeywordAny),
		generatePatternArrayString(fwStringsToAny(likeAny), queryArrayKeywordAny),
		generatePatternArrayString(fwStringsToAny(likeAll), queryArrayKeywordAll),
		generatePatternArrayString(fwStringsToAny(notLikeAll), queryArrayKeywordAll),
		regexPattern,
	}, "_"))

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
