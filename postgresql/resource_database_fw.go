package postgresql

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/lib/pq"
)

var (
	_ resource.Resource                = (*databaseResource)(nil)
	_ resource.ResourceWithConfigure   = (*databaseResource)(nil)
	_ resource.ResourceWithImportState = (*databaseResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*databaseResource)(nil)
)

// NewDatabaseResource returns the postgresql_database resource.
func NewDatabaseResource() resource.Resource {
	return &databaseResource{}
}

type databaseResource struct {
	client *Client
}

type databaseResourceModel struct {
	ID                   types.String `tfsdk:"id"`
	Name                 types.String `tfsdk:"name"`
	Owner                types.String `tfsdk:"owner"`
	Template             types.String `tfsdk:"template"`
	Encoding             types.String `tfsdk:"encoding"`
	Collation            types.String `tfsdk:"lc_collate"`
	CType                types.String `tfsdk:"lc_ctype"`
	TablespaceName       types.String `tfsdk:"tablespace_name"`
	ConnectionLimit      types.Int64  `tfsdk:"connection_limit"`
	AllowConnections     types.Bool   `tfsdk:"allow_connections"`
	IsTemplate           types.Bool   `tfsdk:"is_template"`
	AlterObjectOwnership types.Bool   `tfsdk:"alter_object_ownership"`
}

// ModifyPlan keeps the computed id in sync with name. The database name is
// updatable in place (ALTER DATABASE ... RENAME), and the id equals the name,
// so on a rename the planned id must change too — otherwise the id plan
// modifier (UseStateForUnknown) would keep the old value while apply writes the
// new one, yielding a "provider produced inconsistent result" error.
func (r *databaseResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		// Resource is being destroyed.
		return
	}
	var name types.String
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("name"), &name)...)
	if resp.Diagnostics.HasError() || name.IsUnknown() || name.IsNull() {
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("id"), name)...)
}

func (r *databaseResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_database"
}

func (r *databaseResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Creates and manages a database on a PostgreSQL server.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Synthetic id: the database name",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "The PostgreSQL database name to connect to",
			},
			"owner": schema.StringAttribute{
				Optional:      true,
				Computed:      true,
				Description:   "The ROLE which owns the database",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"template": schema.StringAttribute{
				Optional:      true,
				Computed:      true,
				Default:       stringdefault.StaticString("template0"),
				Description:   "The name of the template from which to create the new database",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"encoding": schema.StringAttribute{
				Optional:      true,
				Computed:      true,
				Default:       stringdefault.StaticString("UTF8"),
				Description:   "Character set encoding to use in the new database",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"lc_collate": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Collation order (LC_COLLATE) to use in the new database",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"lc_ctype": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Character classification (LC_CTYPE) to use in the new database",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"tablespace_name": schema.StringAttribute{
				Optional:      true,
				Computed:      true,
				Description:   "The name of the tablespace that will be associated with the new database",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"connection_limit": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Default:     int64default.StaticInt64(-1),
				Description: "How many concurrent connections can be made to this database",
				Validators:  []validator.Int64{int64validator.AtLeast(-1)},
			},
			"allow_connections": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(true),
				Description: "If false then no one can connect to this database",
			},
			"is_template": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "If true, then this database can be cloned by any user with CREATEDB privileges",
			},
			"alter_object_ownership": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "If true, the owner of already existing objects will change if the owner changes",
			},
		},
	}
}

func (r *databaseResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// dbFwStringOrEmpty returns the empty string for a null or unknown value.
func dbFwStringOrEmpty(v types.String) string {
	if v.IsNull() || v.IsUnknown() {
		return ""
	}
	return v.ValueString()
}

func (r *databaseResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data databaseResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	db, err := r.client.Connect()
	if err != nil {
		resp.Diagnostics.AddError("could not connect to database", err.Error())
		return
	}

	if err := r.createDatabase(db, data); err != nil {
		resp.Diagnostics.AddError("could not create database", err.Error())
		return
	}

	data.ID = types.StringValue(data.Name.ValueString())

	if found := r.readDatabaseInto(db, &data, &resp.Diagnostics); resp.Diagnostics.HasError() {
		return
	} else if !found {
		resp.Diagnostics.AddError("could not create database", "database not found after creation")
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// createDatabase issues CREATE DATABASE. CREATE DATABASE cannot run inside a
// transaction and must be executed against a connection to a *different*
// database than the one being created, so it runs directly against the
// provider connection (db) rather than a txn.
func (r *databaseResource) createDatabase(db *DBConnection, data databaseResourceModel) (err error) {
	currentUser := r.client.config.getDatabaseUsername()
	owner := dbFwStringOrEmpty(data.Owner)

	if owner != "" {
		// Take a lock on db currentUser to avoid multiple database creation at the same time
		// It can fail if they grant the same owner to current at the same time as it's not done in transaction.
		lockTxn, lerr := startTransaction(db.client, "")
		if lerr != nil {
			return lerr
		}
		if err := pgLockRole(lockTxn, currentUser); err != nil {
			return err
		}
		defer deferredRollback(lockTxn)

		// Needed in order to set the owner of the db if the connection user is not a
		// superuser
		ownerGranted, gerr := grantRoleMembership(db, owner, currentUser)
		if gerr != nil {
			return gerr
		}
		if ownerGranted {
			defer func() {
				_, err = revokeRoleMembership(db, owner, currentUser)
			}()
		}
	}

	dbName := data.Name.ValueString()
	b := bytes.NewBufferString("CREATE DATABASE ")
	fmt.Fprint(b, pq.QuoteIdentifier(dbName))

	// Handle each option individually and stream results into the query buffer.
	if owner != "" {
		fmt.Fprint(b, " OWNER ", pq.QuoteIdentifier(owner))
	} else {
		// No owner specified in the config, default to using
		// the connecting username.
		fmt.Fprint(b, " OWNER ", pq.QuoteIdentifier(currentUser))
	}

	template := dbFwStringOrEmpty(data.Template)
	switch {
	case template != "" && strings.ToUpper(template) == "DEFAULT":
		fmt.Fprint(b, " TEMPLATE DEFAULT")
	case template != "":
		fmt.Fprint(b, " TEMPLATE ", pq.QuoteIdentifier(template))
	case template == "":
		fmt.Fprint(b, " TEMPLATE template0")
	}

	encoding := dbFwStringOrEmpty(data.Encoding)
	switch {
	case encoding != "" && strings.ToUpper(encoding) == "DEFAULT":
		fmt.Fprintf(b, " ENCODING DEFAULT")
	case encoding != "":
		fmt.Fprintf(b, " ENCODING '%s' ", pqQuoteLiteral(encoding))
	case encoding == "":
		fmt.Fprint(b, ` ENCODING 'UTF8'`)
	}

	// Don't specify LC_COLLATE if user didn't specify it
	// This will use the default one (usually the one defined in the template database)
	collation := dbFwStringOrEmpty(data.Collation)
	switch {
	case collation != "" && strings.ToUpper(collation) == "DEFAULT":
		fmt.Fprintf(b, " LC_COLLATE DEFAULT")
	case collation != "":
		fmt.Fprintf(b, " LC_COLLATE '%s' ", pqQuoteLiteral(collation))
	}

	// Don't specify LC_CTYPE if user didn't specify it
	// This will use the default one (usually the one defined in the template database)
	ctype := dbFwStringOrEmpty(data.CType)
	switch {
	case ctype != "" && strings.ToUpper(ctype) == "DEFAULT":
		fmt.Fprintf(b, " LC_CTYPE DEFAULT")
	case ctype != "":
		fmt.Fprintf(b, " LC_CTYPE '%s' ", pqQuoteLiteral(ctype))
	}

	tablespace := dbFwStringOrEmpty(data.TablespaceName)
	switch {
	case tablespace != "" && strings.ToUpper(tablespace) == "DEFAULT":
		fmt.Fprint(b, " TABLESPACE DEFAULT")
	case tablespace != "":
		fmt.Fprint(b, " TABLESPACE ", pq.QuoteIdentifier(tablespace))
	}

	if db.featureSupported(featureDBAllowConnections) {
		val := data.AllowConnections.ValueBool()
		fmt.Fprint(b, " ALLOW_CONNECTIONS ", val)
	}

	{
		val := data.ConnectionLimit.ValueInt64()
		fmt.Fprint(b, " CONNECTION LIMIT ", val)
	}

	if db.featureSupported(featureDBIsTemplate) {
		val := data.IsTemplate.ValueBool()
		fmt.Fprint(b, " IS_TEMPLATE ", val)
	}

	sqlStr := b.String()
	if _, err := db.Exec(sqlStr); err != nil {
		return fmt.Errorf("error creating database %q: %w", dbName, err)
	}

	// Set err outside of the return so that the deferred revoke can override err
	// if necessary.
	return err
}

func (r *databaseResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data databaseResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	db, err := r.client.Connect()
	if err != nil {
		resp.Diagnostics.AddError("could not connect to database", err.Error())
		return
	}

	found := r.readDatabaseInto(db, &data, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// readDatabaseInto reads the database attributes from the server into data. It
// returns false if the database no longer exists. The id is used as the lookup
// key (it equals the name).
func (r *databaseResource) readDatabaseInto(db *DBConnection, data *databaseResourceModel, diags *diag.Diagnostics) bool {
	dbID := data.ID.ValueString()
	if dbID == "" {
		dbID = data.Name.ValueString()
	}

	var dbName, ownerName string
	err := db.QueryRow("SELECT d.datname, pg_catalog.pg_get_userbyid(d.datdba) from pg_database d WHERE datname=$1", dbID).Scan(&dbName, &ownerName)
	switch {
	case err == sql.ErrNoRows:
		log.Printf("[WARN] PostgreSQL database (%q) not found", dbID)
		return false
	case err != nil:
		diags.AddError("error reading database", err.Error())
		return false
	}

	var dbEncoding, dbCollation, dbCType, dbTablespaceName string
	var dbConnLimit int

	columns := []string{
		"pg_catalog.pg_encoding_to_char(d.encoding)",
		"d.datcollate",
		"d.datctype",
		"ts.spcname",
		"d.datconnlimit",
	}

	dbSQLFmt := `SELECT %s ` +
		`FROM pg_catalog.pg_database AS d, pg_catalog.pg_tablespace AS ts ` +
		`WHERE d.datname = $1 AND d.dattablespace = ts.oid`
	dbSQL := fmt.Sprintf(dbSQLFmt, strings.Join(columns, ", "))
	err = db.QueryRow(dbSQL, dbID).
		Scan(
			&dbEncoding,
			&dbCollation,
			&dbCType,
			&dbTablespaceName,
			&dbConnLimit,
		)
	switch {
	case err == sql.ErrNoRows:
		log.Printf("[WARN] PostgreSQL database (%q) not found", dbID)
		return false
	case err != nil:
		diags.AddError("error reading database", err.Error())
		return false
	}

	data.Name = types.StringValue(dbName)
	data.ID = types.StringValue(dbName)
	data.Owner = types.StringValue(ownerName)

	// For encoding/lc_collate/lc_ctype/tablespace_name the configured "DEFAULT"
	// sentinel is preserved (the server reports the resolved value, which would
	// otherwise conflict with the configured "DEFAULT").
	if strings.ToUpper(dbFwStringOrEmpty(data.Encoding)) != "DEFAULT" {
		data.Encoding = types.StringValue(dbEncoding)
	}
	if strings.ToUpper(dbFwStringOrEmpty(data.Collation)) != "DEFAULT" {
		data.Collation = types.StringValue(dbCollation)
	}
	if strings.ToUpper(dbFwStringOrEmpty(data.CType)) != "DEFAULT" {
		data.CType = types.StringValue(dbCType)
	}
	if strings.ToUpper(dbFwStringOrEmpty(data.TablespaceName)) != "DEFAULT" {
		data.TablespaceName = types.StringValue(dbTablespaceName)
	}
	data.ConnectionLimit = types.Int64Value(int64(dbConnLimit))

	// template is not read back from the server; preserve the configured value,
	// defaulting empty to template0.
	dbTemplate := dbFwStringOrEmpty(data.Template)
	if dbTemplate == "" {
		dbTemplate = "template0"
	}
	data.Template = types.StringValue(dbTemplate)

	if db.featureSupported(featureDBAllowConnections) {
		var dbAllowConns bool
		dbSQL := fmt.Sprintf(dbSQLFmt, "d.datallowconn")
		err = db.QueryRow(dbSQL, dbID).Scan(&dbAllowConns)
		if err != nil {
			diags.AddError("error reading ALLOW_CONNECTIONS property for DATABASE", err.Error())
			return false
		}
		data.AllowConnections = types.BoolValue(dbAllowConns)
	}

	if db.featureSupported(featureDBIsTemplate) {
		var dbIsTemplate bool
		dbSQL := fmt.Sprintf(dbSQLFmt, "d.datistemplate")
		err = db.QueryRow(dbSQL, dbID).Scan(&dbIsTemplate)
		if err != nil {
			diags.AddError("error reading IS_TEMPLATE property for DATABASE", err.Error())
			return false
		}
		data.IsTemplate = types.BoolValue(dbIsTemplate)
	}

	return true
}

func (r *databaseResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state databaseResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	db, err := r.client.Connect()
	if err != nil {
		resp.Diagnostics.AddError("could not connect to database", err.Error())
		return
	}

	data := plan

	if err := r.setDBName(db, plan, state); err != nil {
		resp.Diagnostics.AddError("could not update database", err.Error())
		return
	}
	if err := r.setAlterOwnership(db, plan, state); err != nil {
		resp.Diagnostics.AddError("could not update database", err.Error())
		return
	}
	if err := r.setDBOwner(db, plan, state); err != nil {
		resp.Diagnostics.AddError("could not update database", err.Error())
		return
	}
	if err := r.setDBTablespace(db, plan, state); err != nil {
		resp.Diagnostics.AddError("could not update database", err.Error())
		return
	}
	if err := r.setDBConnLimit(db, plan, state); err != nil {
		resp.Diagnostics.AddError("could not update database", err.Error())
		return
	}
	if err := r.setDBAllowConns(db, plan, state); err != nil {
		resp.Diagnostics.AddError("could not update database", err.Error())
		return
	}
	if err := r.setDBIsTemplate(db, plan, state); err != nil {
		resp.Diagnostics.AddError("could not update database", err.Error())
		return
	}

	data.ID = types.StringValue(data.Name.ValueString())

	if found := r.readDatabaseInto(db, &data, &resp.Diagnostics); resp.Diagnostics.HasError() {
		return
	} else if !found {
		resp.State.RemoveResource(ctx)
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *databaseResource) setDBName(db *DBConnection, plan, state databaseResourceModel) error {
	o := state.Name.ValueString()
	n := plan.Name.ValueString()
	if o == n {
		return nil
	}
	if n == "" {
		return errors.New("error setting database name to an empty string")
	}

	sqlStr := fmt.Sprintf("ALTER DATABASE %s RENAME TO %s", pq.QuoteIdentifier(o), pq.QuoteIdentifier(n))
	if _, err := db.Exec(sqlStr); err != nil {
		return fmt.Errorf("error updating database name: %w", err)
	}

	return nil
}

func (r *databaseResource) setDBOwner(db *DBConnection, plan, state databaseResourceModel) (err error) {
	if plan.Owner.ValueString() == state.Owner.ValueString() {
		return nil
	}

	owner := plan.Owner.ValueString()
	if owner == "" {
		return nil
	}
	currentUser := r.client.config.getDatabaseUsername()

	lockTxn, lerr := startTransaction(db.client, "")
	if lerr != nil {
		return lerr
	}
	if err := pgLockRole(lockTxn, currentUser); err != nil {
		return err
	}
	defer deferredRollback(lockTxn)

	// needed in order to set the owner of the db if the connection user is not a superuser
	ownerGranted, gerr := grantRoleMembership(db, owner, currentUser)
	if gerr != nil {
		return gerr
	}
	if ownerGranted {
		defer func() {
			_, err = revokeRoleMembership(db, owner, currentUser)
		}()
	}

	dbName := plan.Name.ValueString()

	sqlStr := fmt.Sprintf("ALTER DATABASE %s OWNER TO %s", pq.QuoteIdentifier(dbName), pq.QuoteIdentifier(owner))
	if _, err := db.Exec(sqlStr); err != nil {
		return fmt.Errorf("error updating database OWNER: %w", err)
	}

	return err
}

func (r *databaseResource) setAlterOwnership(db *DBConnection, plan, state databaseResourceModel) (err error) {
	if plan.Owner.ValueString() == state.Owner.ValueString() &&
		plan.AlterObjectOwnership.ValueBool() == state.AlterObjectOwnership.ValueBool() {
		return nil
	}
	owner := plan.Owner.ValueString()
	if owner == "" {
		return nil
	}

	alterOwnership := plan.AlterObjectOwnership.ValueBool()
	if !alterOwnership {
		return nil
	}
	currentUser := r.client.config.getDatabaseUsername()

	dbName := plan.Name.ValueString()

	lockTxn, lerr := startTransaction(db.client, dbName)
	if lerr != nil {
		return lerr
	}
	if err := pgLockRole(lockTxn, currentUser); err != nil {
		return err
	}
	defer deferredRollback(lockTxn)

	currentOwner, err := getDatabaseOwner(db, dbName)
	if err != nil {
		return fmt.Errorf("error getting current database OWNER: %w", err)
	}

	newOwner := plan.Owner.ValueString()

	if currentOwner == newOwner {
		return nil
	}

	currentOwnerGranted, gerr := grantRoleMembership(db, currentOwner, currentUser)
	if gerr != nil {
		return gerr
	}
	if currentOwnerGranted {
		defer func() {
			_, err = revokeRoleMembership(db, currentOwner, currentUser)
		}()
	}
	sqlStr := fmt.Sprintf("REASSIGN OWNED BY %s TO %s", pq.QuoteIdentifier(currentOwner), pq.QuoteIdentifier(newOwner))
	if _, err := lockTxn.Exec(sqlStr); err != nil {
		return fmt.Errorf("error reassigning objects owned by '%s': %w", currentOwner, err)
	}

	if err := lockTxn.Commit(); err != nil {
		return fmt.Errorf("error committing reassign: %w", err)
	}
	return nil
}

func (r *databaseResource) setDBTablespace(db *DBConnection, plan, state databaseResourceModel) error {
	if plan.TablespaceName.ValueString() == state.TablespaceName.ValueString() {
		return nil
	}

	tbspName := plan.TablespaceName.ValueString()
	dbName := plan.Name.ValueString()
	var sqlStr string
	if tbspName == "" || strings.ToUpper(tbspName) == "DEFAULT" {
		sqlStr = fmt.Sprintf("ALTER DATABASE %s RESET TABLESPACE", pq.QuoteIdentifier(dbName))
	} else {
		sqlStr = fmt.Sprintf("ALTER DATABASE %s SET TABLESPACE %s", pq.QuoteIdentifier(dbName), pq.QuoteIdentifier(tbspName))
	}

	if _, err := db.Exec(sqlStr); err != nil {
		return fmt.Errorf("error updating database TABLESPACE: %w", err)
	}

	return nil
}

func (r *databaseResource) setDBConnLimit(db *DBConnection, plan, state databaseResourceModel) error {
	if plan.ConnectionLimit.ValueInt64() == state.ConnectionLimit.ValueInt64() {
		return nil
	}

	connLimit := plan.ConnectionLimit.ValueInt64()
	dbName := plan.Name.ValueString()
	sqlStr := fmt.Sprintf("ALTER DATABASE %s CONNECTION LIMIT = %d", pq.QuoteIdentifier(dbName), connLimit)
	if _, err := db.Exec(sqlStr); err != nil {
		return fmt.Errorf("error updating database CONNECTION LIMIT: %w", err)
	}

	return nil
}

func (r *databaseResource) setDBAllowConns(db *DBConnection, plan, state databaseResourceModel) error {
	if plan.AllowConnections.ValueBool() == state.AllowConnections.ValueBool() {
		return nil
	}

	if !db.featureSupported(featureDBAllowConnections) {
		return fmt.Errorf("PostgreSQL client is talking with a server (%q) that does not support database ALLOW_CONNECTIONS", db.version.String())
	}

	allowConns := plan.AllowConnections.ValueBool()
	dbName := plan.Name.ValueString()
	sqlStr := fmt.Sprintf("ALTER DATABASE %s ALLOW_CONNECTIONS %t", pq.QuoteIdentifier(dbName), allowConns)
	if _, err := db.Exec(sqlStr); err != nil {
		return fmt.Errorf("error updating database ALLOW_CONNECTIONS: %w", err)
	}

	return nil
}

func (r *databaseResource) setDBIsTemplate(db *DBConnection, plan, state databaseResourceModel) error {
	if plan.IsTemplate.ValueBool() == state.IsTemplate.ValueBool() {
		return nil
	}

	if err := fwDoSetDBIsTemplate(db, plan.Name.ValueString(), plan.IsTemplate.ValueBool()); err != nil {
		return fmt.Errorf("error updating database IS_TEMPLATE: %w", err)
	}

	return nil
}

func (r *databaseResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data databaseResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	db, err := r.client.Connect()
	if err != nil {
		resp.Diagnostics.AddError("could not connect to database", err.Error())
		return
	}

	if err := r.deleteDatabase(db, data); err != nil {
		resp.Diagnostics.AddError("could not delete database", err.Error())
		return
	}
}

func (r *databaseResource) deleteDatabase(db *DBConnection, data databaseResourceModel) (err error) {
	currentUser := r.client.config.getDatabaseUsername()
	owner := dbFwStringOrEmpty(data.Owner)

	var dropWithForce string
	if owner != "" {
		lockTxn, lerr := startTransaction(db.client, "")
		if lerr != nil {
			return lerr
		}
		if err := pgLockRole(lockTxn, currentUser); err != nil {
			return err
		}
		defer deferredRollback(lockTxn)

		// Needed in order to set the owner of the db if the connection user is not a
		// superuser
		ownerGranted, gerr := grantRoleMembership(db, owner, currentUser)
		if gerr != nil {
			return gerr
		}
		if ownerGranted {
			defer func() {
				_, err = revokeRoleMembership(db, owner, currentUser)
			}()
		}
	}

	dbName := data.Name.ValueString()
	if db.featureSupported(featureDBIsTemplate) {
		if isTemplate := data.IsTemplate.ValueBool(); isTemplate {
			// Template databases must have this attribute cleared before
			// they can be dropped.
			if err := fwDoSetDBIsTemplate(db, dbName, false); err != nil {
				return fmt.Errorf("error updating database IS_TEMPLATE during DROP DATABASE: %w", err)
			}
		}
	}

	// Terminate all active connections and block new one
	if err := fwTerminateBConnections(db, dbName); err != nil {
		return err
	}

	// Drop with force only for psql 13+
	if db.featureSupported(featureForceDropDatabase) {
		dropWithForce = "WITH ( FORCE )"
	}

	sqlStr := fmt.Sprintf("DROP DATABASE %s %s", pq.QuoteIdentifier(dbName), dropWithForce)
	if _, err := db.Exec(sqlStr); err != nil {
		return fmt.Errorf("error dropping database: %w", err)
	}

	// Returning err even if it's nil so defer func can modify it.
	return err
}

// ImportState accepts the database name (which is also the id).
func (r *databaseResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

// fwDoSetDBIsTemplate sets the IS_TEMPLATE property on the database.
func fwDoSetDBIsTemplate(db *DBConnection, dbName string, isTemplate bool) error {
	if !db.featureSupported(featureDBIsTemplate) {
		return fmt.Errorf("PostgreSQL client is talking with a server (%q) that does not support database IS_TEMPLATE", db.version.String())
	}

	sqlStr := fmt.Sprintf("ALTER DATABASE %s IS_TEMPLATE %t", pq.QuoteIdentifier(dbName), isTemplate)
	if _, err := db.Exec(sqlStr); err != nil {
		return fmt.Errorf("error updating database IS_TEMPLATE: %w", err)
	}

	return nil
}

// fwTerminateBConnections blocks new connections and terminates other backends
// connected to the database.
func fwTerminateBConnections(db *DBConnection, dbName string) error {
	var terminateSql string

	if db.featureSupported(featureDBAllowConnections) {
		alterSql := fmt.Sprintf("ALTER DATABASE %s ALLOW_CONNECTIONS false", pq.QuoteIdentifier(dbName))

		if _, err := db.Exec(alterSql); err != nil {
			return fmt.Errorf("error blocking connections to database: %w", err)
		}
	}
	pid := "procpid"
	if db.featureSupported(featurePid) {
		pid = "pid"
	}
	terminateSql = fmt.Sprintf("SELECT pg_terminate_backend(%s) FROM pg_stat_activity WHERE datname = '%s' AND %s <> pg_backend_pid()", pid, dbName, pid)
	if _, err := db.Exec(terminateSql); err != nil {
		return fmt.Errorf("error terminating database connections: %w", err)
	}

	return nil
}
