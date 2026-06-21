package postgresql

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/lib/pq"
)

var (
	_ resource.Resource                = (*functionResource)(nil)
	_ resource.ResourceWithConfigure   = (*functionResource)(nil)
	_ resource.ResourceWithImportState = (*functionResource)(nil)
)

// Default values for function attributes used when the configuration leaves
// them unset.
const (
	defaultFunctionVolatility = "VOLATILE"
	defaultFunctionParallel   = "UNSAFE"
)

// NewFunctionResource returns the postgresql_function resource.
func NewFunctionResource() resource.Resource {
	return &functionResource{}
}

type functionResource struct {
	client *Client
}

type functionArgModel struct {
	Type    types.String `tfsdk:"type"`
	Name    types.String `tfsdk:"name"`
	Mode    types.String `tfsdk:"mode"`
	Default types.String `tfsdk:"default"`
}

type functionResourceModel struct {
	ID              types.String       `tfsdk:"id"`
	Schema          types.String       `tfsdk:"schema"`
	Name            types.String       `tfsdk:"name"`
	Arg             []functionArgModel `tfsdk:"arg"`
	Language        types.String       `tfsdk:"language"`
	Returns         types.String       `tfsdk:"returns"`
	Body            types.String       `tfsdk:"body"`
	DropCascade     types.Bool         `tfsdk:"drop_cascade"`
	Parallel        types.String       `tfsdk:"parallel"`
	SecurityDefiner types.Bool         `tfsdk:"security_definer"`
	Strict          types.Bool         `tfsdk:"strict"`
	Volatility      types.String       `tfsdk:"volatility"`
	Database        types.String       `tfsdk:"database"`
}

func (r *functionResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_function"
}

func (r *functionResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a PostgreSQL function.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Synthetic id: database.schema.name(argument types)",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"schema": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Schema where the function is located. If not specified, the provider default schema is used.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Required:      true,
				Description:   "Name of the function.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"language": schema.StringAttribute{
				Optional:      true,
				Computed:      true,
				Default:       stringdefault.StaticString("plpgsql"),
				Description:   "Language of the function. One of: internal, sql, c, plpgsql",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"returns": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Function return type. If not specified, it will be calculated based on the output arguments",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"body": schema.StringAttribute{
				Required:    true,
				Description: "Body of the function.",
			},
			"drop_cascade": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "Automatically drop objects that depend on the function (such as operators or triggers), and in turn all objects that depend on those objects.",
			},
			"parallel": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString(defaultFunctionParallel),
				Description: "If the function can be executed in parallel for a single query execution. One of: UNSAFE, RESTRICTED, SAFE",
			},
			"security_definer": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "If the function should execute with the permissions of the function owner instead of the permissions of the caller.",
			},
			"strict": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "If the function should always return NULL if any of it's inputs is NULL.",
			},
			"volatility": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString(defaultFunctionVolatility),
				Description: "Volatility of the function. One of: VOLATILE, STABLE, IMMUTABLE.",
			},
			"database": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "The database where the function is located. If not specified, the provider default database is used.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
		},
		Blocks: map[string]schema.Block{
			"arg": schema.ListNestedBlock{
				Description: "Function argument definitions.",
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"type": schema.StringAttribute{
							Required:    true,
							Description: "The argument type.",
						},
						"name": schema.StringAttribute{
							Optional:    true,
							Description: "The argument name. The name may be required for some languages or depending on the argument mode.",
						},
						"mode": schema.StringAttribute{
							Optional:    true,
							Computed:    true,
							Default:     stringdefault.StaticString("IN"),
							Description: "The argument mode. One of: IN, OUT, INOUT, or VARIADIC",
						},
						"default": schema.StringAttribute{
							Optional:    true,
							Description: "An expression to be used as default value if the parameter is not specified.",
						},
					},
				},
				PlanModifiers: []planmodifier.List{listplanmodifier.RequiresReplace()},
			},
		},
	}
}

func (r *functionResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// requireFeature verifies the connected PostgreSQL version supports functions.
func (r *functionResource) requireFeature(diags *diag.Diagnostics) bool {
	db, err := r.client.Connect()
	if err != nil {
		diags.AddError("could not connect to database", err.Error())
		return false
	}
	if !db.featureSupported(featureFunction) {
		diags.AddError(
			"feature not supported",
			fmt.Sprintf("postgresql_function resource is not supported for this Postgres version (%s)", db.version),
		)
		return false
	}
	return true
}

// resolveDatabase returns the database to operate against (config value or the
// provider default).
func (r *functionResource) resolveDatabase(m functionResourceModel) string {
	if m.Database.IsNull() || m.Database.IsUnknown() || m.Database.ValueString() == "" {
		return r.client.DatabaseName()
	}
	return m.Database.ValueString()
}

// functionModelToPG builds a PGFunction from the resource model, applying
// default values for unset attributes.
func functionModelToPG(m functionResourceModel) PGFunction {
	var f PGFunction

	if m.Schema.ValueString() != "" {
		f.Schema = m.Schema.ValueString()
	} else {
		f.Schema = "public"
	}

	f.Name = m.Name.ValueString()

	if m.Language.ValueString() != "" {
		f.Language = m.Language.ValueString()
	} else {
		f.Language = "plpgsql"
	}

	f.Body = normalizeFunctionBody(m.Body.ValueString())

	if m.Parallel.ValueString() != "" {
		f.Parallel = m.Parallel.ValueString()
	} else {
		f.Parallel = defaultFunctionParallel
	}

	f.Strict = m.Strict.ValueBool()
	f.SecurityDefiner = m.SecurityDefiner.ValueBool()

	if m.Volatility.ValueString() != "" {
		f.Volatility = m.Volatility.ValueString()
	} else {
		f.Volatility = defaultFunctionVolatility
	}

	// Default return type when not provided, derived from the OUT argument.
	argOutput := "void"

	f.Args = []PGFunctionArg{}
	for _, a := range m.Arg {
		pgArg := PGFunctionArg{
			Mode:    a.Mode.ValueString(),
			Name:    a.Name.ValueString(),
			Type:    a.Type.ValueString(),
			Default: a.Default.ValueString(),
		}

		if strings.ToUpper(pgArg.Mode) == "OUT" {
			argOutput = pgArg.Type
		}

		f.Args = append(f.Args, pgArg)
	}

	if m.Returns.ValueString() != "" {
		f.Returns = m.Returns.ValueString()
	} else {
		f.Returns = argOutput
	}

	return f
}

// buildCreateFunctionSQL renders the CREATE [OR REPLACE] FUNCTION statement.
func buildCreateFunctionSQL(f PGFunction, replace bool) string {
	b := bytes.NewBufferString("CREATE ")

	if replace {
		b.WriteString(" OR REPLACE ")
	}

	b.WriteString("FUNCTION ")

	fmt.Fprint(b, pq.QuoteIdentifier(f.Schema), ".")
	fmt.Fprint(b, pq.QuoteIdentifier(f.Name), " (")

	for i, arg := range f.Args {
		if i > 0 {
			b.WriteRune(',')
		}

		b.WriteString("\n    ")

		if arg.Mode != "" {
			fmt.Fprint(b, arg.Mode, " ")
		}

		if arg.Name != "" {
			fmt.Fprint(b, arg.Name, " ")
		}

		b.WriteString(arg.Type)

		if arg.Default != "" {
			fmt.Fprint(b, " DEFAULT ", arg.Default)
		}
	}

	if len(f.Args) > 0 {
		b.WriteRune('\n')
	}

	b.WriteString(")")

	fmt.Fprint(b, "\nRETURNS ", f.Returns)
	fmt.Fprint(b, "\nLANGUAGE ", f.Language)
	if f.Volatility != defaultFunctionVolatility {
		fmt.Fprint(b, "\n", f.Volatility)
	}
	if f.SecurityDefiner {
		fmt.Fprint(b, "\nSECURITY DEFINER")
	}
	if f.Parallel != defaultFunctionParallel {
		fmt.Fprint(b, "\nPARALLEL ", f.Parallel)
	}
	if f.Strict {
		fmt.Fprint(b, "\nSTRICT")
	}

	fmt.Fprint(b, "\nAS $function$", f.Body, "$function$;")

	return b.String()
}

// generateFunctionIDFw produces "database.schema.name(argument types)" where OUT
// arguments are excluded and the remaining argument types are joined by commas
// (no spaces).
func generateFunctionIDFw(f PGFunction, database string) string {
	b := bytes.NewBufferString("")

	fmt.Fprint(b, database, ".")
	fmt.Fprint(b, f.Schema, ".", f.Name, "(")

	argCount := 0
	for _, arg := range f.Args {
		mode := "IN"
		if arg.Mode != "" {
			mode = arg.Mode
		}

		if mode != "OUT" {
			if argCount > 0 {
				b.WriteRune(',')
			}

			b.WriteString(arg.Type)
			argCount++
		}
	}

	b.WriteRune(')')

	return b.String()
}

// quoteSignatureFw quotes the schema and name in a function signature of the
// form schema.name(arguments).
func quoteSignatureFw(s string) (string, error) {
	signatureData := findStringSubmatchMap(`(?si)(?P<Schema>[^\.]+)\.(?P<Name>[^(]+)\((?P<Args>.*)\)`, s)

	schemaName, schemaFound := signatureData["Schema"]
	name, nameFound := signatureData["Name"]
	args, argsFound := signatureData["Args"]
	if schemaFound && nameFound && argsFound {
		return fmt.Sprintf("%s.%s(%s)", pq.QuoteIdentifier(schemaName), pq.QuoteIdentifier(name), args), nil
	}

	return "", fmt.Errorf("incorrect signature format \"%s\". The expected format is schema.function_name(arguments)", s)
}

// expandFunctionIDFw splits a function ID into its database name and quoted
// signature. database is the configured database value ("" when not set); client
// may be nil.
func expandFunctionIDFw(functionID string, database string, client *Client) (databaseName string, functionSignature string, err error) {
	partsCount := strings.Count(functionID, ".") + 1

	if partsCount == 2 {
		clientDatabaseName := "postgres"
		if client != nil {
			clientDatabaseName = client.databaseName
		}

		signature, err := quoteSignatureFw(functionID)
		if err != nil {
			return "", "", err
		}

		if database != "" {
			clientDatabaseName = database
		}

		return clientDatabaseName, signature, nil
	}

	if partsCount == 3 {
		functionIDParts := strings.Split(functionID, ".")
		signature, err := quoteSignatureFw(strings.Join(functionIDParts[1:], "."))
		if err != nil {
			return "", "", err
		}
		return functionIDParts[0], signature, nil
	}

	return "", "", fmt.Errorf("function ID %s has not the expected format 'database.schema.function_name(arguments)'", functionID)
}

func (r *functionResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data functionResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !r.requireFeature(&resp.Diagnostics) {
		return
	}

	f := functionModelToPG(data)
	sqlStmt := buildCreateFunctionSQL(f, false)

	// Run the statement against the configured database value ("" => provider
	// default).
	txn, err := startTransaction(r.client, data.Database.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	if _, err := txn.Exec(sqlStmt); err != nil {
		resp.Diagnostics.AddError("could not create function", err.Error())
		return
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("could not commit function", err.Error())
		return
	}

	database := r.resolveDatabase(data)

	data.Database = types.StringValue(database)
	data.Schema = types.StringValue(f.Schema)
	data.Language = types.StringValue(f.Language)
	data.Returns = types.StringValue(f.Returns)
	data.Parallel = types.StringValue(f.Parallel)
	data.Volatility = types.StringValue(f.Volatility)
	data.Strict = types.BoolValue(f.Strict)
	data.SecurityDefiner = types.BoolValue(f.SecurityDefiner)
	data.ID = types.StringValue(generateFunctionIDFw(f, database))

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *functionResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data functionResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !r.requireFeature(&resp.Diagnostics) {
		return
	}

	functionID := data.ID.ValueString()

	databaseName, functionSignature, err := expandFunctionIDFw(functionID, data.Database.ValueString(), r.client)
	if err != nil {
		resp.Diagnostics.AddError("invalid function id", err.Error())
		return
	}

	query := `SELECT pg_get_functiondef(p.oid::regproc) funcDefinition ` +
		`FROM pg_proc p ` +
		`LEFT JOIN pg_namespace n ON p.pronamespace = n.oid ` +
		`WHERE p.oid = to_regprocedure($1)`

	txn, err := startTransaction(r.client, databaseName)
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	var funcDefinition string
	err = txn.QueryRow(query, functionSignature).Scan(&funcDefinition)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		resp.State.RemoveResource(ctx)
		return
	case err != nil:
		resp.Diagnostics.AddError("error reading function", err.Error())
		return
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("could not commit function read", err.Error())
		return
	}

	var pgFunction PGFunction
	if err := pgFunction.Parse(funcDefinition); err != nil {
		resp.Diagnostics.AddError("could not parse function definition", err.Error())
		return
	}

	data.Database = types.StringValue(databaseName)
	data.Name = types.StringValue(pgFunction.Name)
	data.Schema = types.StringValue(pgFunction.Schema)
	data.Language = types.StringValue(pgFunction.Language)
	data.Returns = types.StringValue(pgFunction.Returns)
	data.Volatility = types.StringValue(pgFunction.Volatility)
	data.Parallel = types.StringValue(pgFunction.Parallel)
	data.Strict = types.BoolValue(pgFunction.Strict)
	data.SecurityDefiner = types.BoolValue(pgFunction.SecurityDefiner)
	data.ID = types.StringValue(functionID)

	// body and arg are config-driven: preserve the existing state values on
	// refresh and only populate them from the database when absent (import).
	if data.Body.IsNull() || data.Body.ValueString() == "" {
		data.Body = types.StringValue(pgFunction.Body)
	}
	if len(data.Arg) == 0 && len(pgFunction.Args) > 0 {
		args := make([]functionArgModel, 0, len(pgFunction.Args))
		for _, a := range pgFunction.Args {
			am := functionArgModel{
				Type: types.StringValue(a.Type),
				Mode: types.StringValue(a.Mode),
			}
			if a.Name != "" {
				am.Name = types.StringValue(a.Name)
			} else {
				am.Name = types.StringNull()
			}
			if a.Default != "" {
				am.Default = types.StringValue(a.Default)
			} else {
				am.Default = types.StringNull()
			}
			args = append(args, am)
		}
		data.Arg = args
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *functionResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data functionResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !r.requireFeature(&resp.Diagnostics) {
		return
	}

	f := functionModelToPG(data)
	sqlStmt := buildCreateFunctionSQL(f, true)

	txn, err := startTransaction(r.client, data.Database.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	if _, err := txn.Exec(sqlStmt); err != nil {
		resp.Diagnostics.AddError("could not update function", err.Error())
		return
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("could not commit function update", err.Error())
		return
	}

	database := r.resolveDatabase(data)

	data.Database = types.StringValue(database)
	data.Schema = types.StringValue(f.Schema)
	data.Language = types.StringValue(f.Language)
	data.Returns = types.StringValue(f.Returns)
	data.Parallel = types.StringValue(f.Parallel)
	data.Volatility = types.StringValue(f.Volatility)
	data.Strict = types.BoolValue(f.Strict)
	data.SecurityDefiner = types.BoolValue(f.SecurityDefiner)
	data.ID = types.StringValue(generateFunctionIDFw(f, database))

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *functionResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data functionResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !r.requireFeature(&resp.Diagnostics) {
		return
	}

	databaseName, functionSignature, err := expandFunctionIDFw(data.ID.ValueString(), data.Database.ValueString(), r.client)
	if err != nil {
		resp.Diagnostics.AddError("invalid function id", err.Error())
		return
	}

	dropMode := "RESTRICT"
	if data.DropCascade.ValueBool() {
		dropMode = "CASCADE"
	}

	sqlStmt := fmt.Sprintf("DROP FUNCTION IF EXISTS %s %s", functionSignature, dropMode)

	txn, err := startTransaction(r.client, databaseName)
	if err != nil {
		resp.Diagnostics.AddError("could not start transaction", err.Error())
		return
	}
	defer deferredRollback(txn)

	if _, err := txn.Exec(sqlStmt); err != nil {
		resp.Diagnostics.AddError("could not drop function", err.Error())
		return
	}

	if err := txn.Commit(); err != nil {
		resp.Diagnostics.AddError("could not commit function deletion", err.Error())
	}
}

// ImportState accepts the function id in the form
// "database.schema.function_name(arguments)". The remaining attributes are
// populated by Read.
func (r *functionResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
