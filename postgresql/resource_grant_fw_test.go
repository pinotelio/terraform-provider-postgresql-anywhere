package postgresql

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

func grantFwCheckDatabasesPrivileges(t *testing.T, canCreate bool) func(*terraform.State) error {
	return func(*terraform.State) error {
		db := connectAsTestRole(t, "test_grant_role", "test_grant_db")
		defer closeDB(t, db)

		return testHasGrantForQuery(db, "CREATE SCHEMA plop", canCreate)
	}
}

func grantFwCheckSchemaPrivileges(t *testing.T, usage, create bool) func(*terraform.State) error {
	return func(*terraform.State) error {
		config := getTestConfig(t)
		dsn := config.connStr("postgres")

		// Create a table in the schema to check if user has usage privilege
		dbExecute(t, dsn, "CREATE TABLE IF NOT EXISTS test_schema.test_usage (id serial)")
		defer func() {
			dbExecute(t, dsn, "DROP TABLE IF EXISTS test_schema.test_create")
		}()
		dbExecute(t, dsn, "GRANT SELECT ON test_schema.test_usage TO test_grant_role")

		db := connectAsTestRole(t, "test_grant_role", "postgres")
		defer closeDB(t, db)

		if err := testHasGrantForQuery(db, "SELECT 1 FROM test_schema.test_usage", usage); err != nil {
			return err
		}

		return testHasGrantForQuery(db, "CREATE TABLE test_schema.test_create (id serial)", create)
	}
}

func grantFwCheckFunctionExecutable(t *testing.T, role, function string) func(*terraform.State) error {
	return func(*terraform.State) error {
		db := connectAsTestRole(t, role, "postgres")
		defer closeDB(t, db)

		return testHasGrantForQuery(db, fmt.Sprintf("SELECT %s()", function), true)
	}
}

func TestAccGrantFW_Table(t *testing.T) {
	skipIfNotAcc(t)

	// We have to create the database outside of resource.Test because we need to
	// create tables to assert that grants are correctly applied.
	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	testTables := []string{"test_schema.test_table", "test_schema.test_table2"}
	createTestTables(t, dbSuffix, testTables, "")

	dbName, roleName := getTestDBNames(dbSuffix)

	var testGrant = fmt.Sprintf(`
	resource "postgresql_grant" "test" {
		database    = "%s"
		role        = "%s"
		schema      = "test_schema"
		object_type = "table"
		privileges  = %%s
	}
	`, dbName, roleName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(testGrant, `["SELECT"]`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(
						"postgresql_grant.test", "id", fmt.Sprintf("%s_%s_test_schema_table", roleName, dbName),
					),
					resource.TestCheckResourceAttr("postgresql_grant.test", "privileges.#", "1"),
					resource.TestCheckResourceAttr("postgresql_grant.test", "privileges.0", "SELECT"),
					func(*terraform.State) error {
						return testCheckTablesPrivileges(t, dbName, roleName, testTables, []string{"SELECT"})
					},
				),
			},
			{
				Config: fmt.Sprintf(testGrant, `["SELECT", "INSERT", "UPDATE"]`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_grant.test", "privileges.#", "3"),
					func(*terraform.State) error {
						return testCheckTablesPrivileges(t, dbName, roleName, testTables, []string{"SELECT", "INSERT", "UPDATE"})
					},
				),
			},
			// Reapply the first step to be sure that extra privileges are revoked.
			{
				Config: fmt.Sprintf(testGrant, `["SELECT"]`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_grant.test", "privileges.#", "1"),
					resource.TestCheckResourceAttr("postgresql_grant.test", "privileges.0", "SELECT"),
					func(*terraform.State) error {
						return testCheckTablesPrivileges(t, dbName, roleName, testTables, []string{"SELECT"})
					},
				),
			},
			// Revoke everything.
			{
				Config: fmt.Sprintf(testGrant, `[]`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_grant.test", "privileges.#", "0"),
					func(*terraform.State) error {
						return testCheckTablesPrivileges(t, dbName, roleName, testTables, []string{})
					},
				),
			},
		},
	})
}

func TestAccGrantFW_Columns(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	testTables := []string{"test_schema.test_table"}
	createTestTables(t, dbSuffix, testTables, "")

	dbName, roleName := getTestDBNames(dbSuffix)

	var testGrant = fmt.Sprintf(`
	resource "postgresql_grant" "test" {
		database    = "%s"
		role        = "%s"
		schema      = "test_schema"
		object_type = "column"
		objects     = ["test_table"]
		columns     = %%s
		privileges  = %%s
	}
	`, dbName, roleName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(testGrant, `["test_column_one", "test_column_two"]`, `["SELECT"]`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_grant.test", "objects.#", "1"),
					resource.TestCheckResourceAttr("postgresql_grant.test", "objects.0", "test_table"),
					resource.TestCheckResourceAttr("postgresql_grant.test", "columns.#", "2"),
					resource.TestCheckResourceAttr("postgresql_grant.test", "privileges.#", "1"),
					resource.TestCheckResourceAttr("postgresql_grant.test", "privileges.0", "SELECT"),
					func(*terraform.State) error {
						return testCheckColumnPrivileges(t, dbName, roleName, []string{testTables[0]}, []string{"SELECT"}, []string{"test_column_one"})
					},
					func(*terraform.State) error {
						return testCheckColumnPrivileges(t, dbName, roleName, []string{testTables[0]}, []string{"SELECT"}, []string{"test_column_one", "test_column_two"})
					},
				),
			},
			{
				Config: fmt.Sprintf(testGrant, `["test_column_one", "test_column_two"]`, `["INSERT"]`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_grant.test", "columns.#", "2"),
					resource.TestCheckResourceAttr("postgresql_grant.test", "privileges.#", "1"),
					resource.TestCheckResourceAttr("postgresql_grant.test", "privileges.0", "INSERT"),
					func(*terraform.State) error {
						return testCheckColumnPrivileges(t, dbName, roleName, []string{testTables[0]}, []string{"INSERT"}, []string{`"test_column_one"`})
					},
				),
			},
		},
	})
}

func TestAccGrantFW_Objects(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	testTables := []string{"test_schema.test_table", "test_schema.test_table2"}
	createTestTables(t, dbSuffix, testTables, "")

	dbName, roleName := getTestDBNames(dbSuffix)

	var testGrant = fmt.Sprintf(`
	resource "postgresql_grant" "test" {
		database    = "%s"
		role        = "%s"
		schema      = "test_schema"
		object_type = "table"
		objects     = %%s
		privileges  = ["SELECT"]
	}
	`, dbName, roleName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(testGrant, `["test_table"]`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(
						"postgresql_grant.test", "id", fmt.Sprintf("%s_%s_test_schema_table_test_table", roleName, dbName),
					),
					resource.TestCheckResourceAttr("postgresql_grant.test", "objects.#", "1"),
					func(*terraform.State) error {
						return testCheckTablesPrivileges(t, dbName, roleName, []string{testTables[0]}, []string{"SELECT"})
					},
					func(*terraform.State) error {
						return testCheckTablesPrivileges(t, dbName, roleName, []string{testTables[1]}, []string{})
					},
				),
			},
			{
				Config: fmt.Sprintf(testGrant, `["test_table", "test_table2"]`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_grant.test", "objects.#", "2"),
					func(*terraform.State) error {
						return testCheckTablesPrivileges(t, dbName, roleName, testTables, []string{"SELECT"})
					},
				),
			},
			{
				// Empty list means that privileges will be applied on all tables.
				Config: fmt.Sprintf(testGrant, `[]`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_grant.test", "objects.#", "0"),
					func(*terraform.State) error {
						return testCheckTablesPrivileges(t, dbName, roleName, testTables, []string{"SELECT"})
					},
				),
			},
			{
				Config:  fmt.Sprintf(testGrant, `[]`),
				Destroy: true,
				Check: resource.ComposeTestCheckFunc(
					func(*terraform.State) error {
						return testCheckTablesPrivileges(t, dbName, roleName, testTables, []string{})
					},
				),
			},
		},
	})
}

func TestAccGrantFW_EmptyPrivileges(t *testing.T) {
	skipIfNotAcc(t)

	config := getTestConfig(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	testTables := []string{"test_schema.test_table", "test_schema.test_table2"}
	createTestTables(t, dbSuffix, testTables, "")

	dbName, roleName := getTestDBNames(dbSuffix)

	// Grant some privileges to assert that they will be revoked.
	dbExecute(t, config.connStr(dbName), fmt.Sprintf("GRANT SELECT, INSERT ON ALL TABLES IN SCHEMA test_schema TO %s", roleName))

	var tfConfig = fmt.Sprintf(`
	resource "postgresql_grant" "test" {
		database    = "%s"
		role        = "%s"
		schema      = "test_schema"
		object_type = "table"
		privileges  = []
	}
	`, dbName, roleName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: tfConfig,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(
						"postgresql_grant.test", "id", fmt.Sprintf("%s_%s_test_schema_table", roleName, dbName),
					),
					resource.TestCheckResourceAttr("postgresql_grant.test", "privileges.#", "0"),
					func(*terraform.State) error {
						return testCheckTablesPrivileges(t, dbName, roleName, testTables, []string{})
					},
				),
			},
		},
	})
}

func TestAccGrantFW_Public(t *testing.T) {
	skipIfNotAcc(t)

	config := getTestConfig(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	testTables := []string{"test_schema.test_table"}
	createTestTables(t, dbSuffix, testTables, "")

	dbName, roleName := getTestDBNames(dbSuffix)

	// create another role to assert that PUBLIC is applied to everyone.
	role2 := fmt.Sprintf("tf_tests_role2_%s", dbSuffix)
	createTestRole(t, role2)
	dbExecute(t, config.connStr(dbName), fmt.Sprintf("GRANT usage ON SCHEMA test_schema to %s", role2))

	var testGrant = fmt.Sprintf(`
	resource "postgresql_grant" "test" {
		database    = "%s"
		role        = "public"
		schema      = "test_schema"
		object_type = "table"
		privileges  = %%s
	}
	`, dbName)

	checkTablePrivileges := func(expectedPrivileges []string) error {
		if err := testCheckTablesPrivileges(t, dbName, roleName, testTables, expectedPrivileges); err != nil {
			return err
		}
		return testCheckTablesPrivileges(t, dbName, role2, testTables, expectedPrivileges)
	}

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(testGrant, `["SELECT"]`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(
						"postgresql_grant.test", "id", fmt.Sprintf("public_%s_test_schema_table", dbName),
					),
					resource.TestCheckResourceAttr("postgresql_grant.test", "privileges.#", "1"),
					func(*terraform.State) error {
						return checkTablePrivileges([]string{"SELECT"})
					},
				),
			},
			{
				Config: fmt.Sprintf(testGrant, `[]`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_grant.test", "privileges.#", "0"),
					func(*terraform.State) error {
						return checkTablePrivileges([]string{})
					},
				),
			},
		},
	})
}

func TestAccGrantFW_ImplicitGrants(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	testTables := []string{"test_schema.test_table"}
	createTestTables(t, dbSuffix, testTables, "")

	dbName, roleName := getTestDBNames(dbSuffix)

	var testGrant = fmt.Sprintf(`
	resource "postgresql_grant" "test" {
		database    = "%s"
		role        = "%s"
		schema      = "test_schema"
		object_type = "table"
		objects     = ["test_table"]
		privileges  = %%s
	}
	`, dbName, roleName)

	var testCheckTableGrants = func(grants ...string) resource.TestCheckFunc {
		return func(*terraform.State) error {
			return testCheckTablesPrivileges(t, dbName, roleName, []string{testTables[0]}, grants)
		}
	}

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(testGrant, `["ALL"]`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(
						"postgresql_grant.test", "id", fmt.Sprintf("%s_%s_test_schema_table_test_table", roleName, dbName),
					),
					resource.TestCheckResourceAttr("postgresql_grant.test", "privileges.#", "1"),
					testCheckTableGrants("SELECT", "INSERT", "UPDATE", "DELETE"),
				),
			},
			{
				Config: fmt.Sprintf(testGrant, `["SELECT"]`),
				Check: resource.ComposeTestCheckFunc(
					testCheckTableGrants("SELECT"),
				),
			},
		},
	})
}

func TestAccGrantFW_ObjectsError(t *testing.T) {
	skipIfNotAcc(t)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `resource "postgresql_grant" "test" {
					database    = "test_db"
					role        = "test_role"
					object_type = "database"
					objects     = ["o1", "o2"]
					privileges  = ["CONNECT"]
				}`,
				ExpectError: regexp.MustCompile("cannot specify `objects` when `object_type` is `database` or `schema`"),
			},
			{
				Config: `resource "postgresql_grant" "test" {
					database    = "test_db"
					schema      = "test_schema"
					role        = "test_role"
					object_type = "schema"
					objects     = ["o1", "o2"]
					privileges  = ["CONNECT"]
				}`,
				ExpectError: regexp.MustCompile("cannot specify `objects` when `object_type` is `database` or `schema`"),
			},
			{
				Config: `resource "postgresql_grant" "test" {
					database    = "test_db"
					role        = "test_role"
					object_type = "foreign_data_wrapper"
					objects     = ["o1", "o2"]
					privileges  = ["USAGE"]
				}`,
				ExpectError: regexp.MustCompile("one element must be specified in `objects` when `object_type` is `foreign_data_wrapper` or `foreign_server`"),
			},
			{
				Config: `resource "postgresql_grant" "test" {
					database    = "test_db"
					role        = "test_role"
					object_type = "foreign_server"
					objects     = ["o1", "o2"]
					privileges  = ["USAGE"]
				}`,
				ExpectError: regexp.MustCompile("one element must be specified in `objects` when `object_type` is `foreign_data_wrapper` or `foreign_server`"),
			},
		},
	})
}

func TestAccGrantFW_ColumnsError(t *testing.T) {
	skipIfNotAcc(t)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `resource "postgresql_grant" "test" {
					database    = "test_db"
					role        = "test_role"
					schema      = "test_schema"
					object_type = "column"
					objects     = ["o1", "o2"]
					columns     = ["col1", "col2"]
					privileges  = ["SELECT"]
				}`,
				ExpectError: regexp.MustCompile("must specify exactly 1 table in the `objects` field when `object_type` is `column`"),
			},
			{
				Config: `resource "postgresql_grant" "test" {
					database    = "test_db"
					role        = "test_role"
					schema      = "test_schema"
					object_type = "column"
					objects     = ["o1"]
					columns     = ["col1", "col2"]
					privileges  = ["SELECT", "INSERT"]
				}`,
				ExpectError: regexp.MustCompile("must specify exactly 1 `privileges` when `object_type` is `column`"),
			},
			{
				Config: `resource "postgresql_grant" "test" {
					database    = "test_db"
					role        = "test_role"
					schema      = "test_schema"
					object_type = "column"
					objects     = ["o1"]
					privileges  = ["SELECT"]
				}`,
				ExpectError: regexp.MustCompile("must specify `columns` when `object_type` is `column`"),
			},
			{
				Config: `resource "postgresql_grant" "test" {
					database    = "test_db"
					role        = "test_role"
					schema      = "test_schema"
					object_type = "table"
					objects     = ["o1"]
					columns     = ["col1", "col2"]
					privileges  = ["SELECT"]
				}`,
				ExpectError: regexp.MustCompile("cannot specify `columns` when `object_type` is not `column`"),
			},
		},
	})
}

func TestAccGrantFW_Database(t *testing.T) {
	skipIfNotAcc(t)

	config := fmt.Sprintf(`
resource "postgresql_role" "test" {
	name     = "test_grant_role"
	password = "%s"
	login    = true
}

resource "postgresql_database" "test_db" {
	depends_on = [postgresql_role.test]
	name = "test_grant_db"
}

resource "postgresql_grant" "test" {
	database    = postgresql_database.test_db.name
	role        = postgresql_role.test.name
	object_type = "database"
	privileges  = %%s
}
`, testRolePassword)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(config, `["CONNECT"]`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_grant.test", "id", "test_grant_role_test_grant_db_database"),
					resource.TestCheckResourceAttr("postgresql_grant.test", "privileges.#", "1"),
					resource.TestCheckResourceAttr("postgresql_grant.test", "with_grant_option", "false"),
					grantFwCheckDatabasesPrivileges(t, false),
				),
			},
			{
				Config: fmt.Sprintf(config, `["CONNECT", "CREATE"]`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_grant.test", "privileges.#", "2"),
					grantFwCheckDatabasesPrivileges(t, true),
				),
			},
			{
				Config: fmt.Sprintf(config, "[]"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_grant.test", "privileges.#", "0"),
					grantFwCheckDatabasesPrivileges(t, false),
				),
			},
		},
	})
}

func TestAccGrantFW_Schema(t *testing.T) {
	skipIfNotAcc(t)

	config := fmt.Sprintf(`
resource "postgresql_role" "test" {
	name     = "test_grant_role"
	password = "%s"
	login    = true
}

resource "postgresql_schema" "test_schema" {
	depends_on   = [postgresql_role.test]
	name         = "test_schema"
	drop_cascade = true
}

resource "postgresql_grant" "test" {
	database    = "postgres"
	schema      = postgresql_schema.test_schema.name
	role        = postgresql_role.test.name
	object_type = "schema"
	privileges  = %%s
}
`, testRolePassword)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(config, `["USAGE"]`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_grant.test", "id", "test_grant_role_postgres_test_schema_schema"),
					resource.TestCheckResourceAttr("postgresql_grant.test", "privileges.#", "1"),
					resource.TestCheckResourceAttr("postgresql_grant.test", "with_grant_option", "false"),
					grantFwCheckSchemaPrivileges(t, true, false),
				),
			},
			{
				Config: fmt.Sprintf(config, `["USAGE", "CREATE"]`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_grant.test", "privileges.#", "2"),
					grantFwCheckSchemaPrivileges(t, true, true),
				),
			},
			{
				Config: fmt.Sprintf(config, `[]`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_grant.test", "privileges.#", "0"),
					grantFwCheckSchemaPrivileges(t, false, false),
				),
			},
		},
	})
}

func TestAccGrantFW_Function(t *testing.T) {
	skipIfNotAcc(t)

	config := getTestConfig(t)
	dsn := config.connStr("postgres")

	// Create a test role and a schema as public has too wide open privileges.
	dbExecute(t, dsn, fmt.Sprintf("CREATE ROLE test_role LOGIN PASSWORD '%s'", testRolePassword))
	dbExecute(t, dsn, "CREATE SCHEMA test_schema")
	dbExecute(t, dsn, "GRANT USAGE ON SCHEMA test_schema TO test_role")
	dbExecute(t, dsn, "ALTER DEFAULT PRIVILEGES REVOKE ALL ON FUNCTIONS FROM PUBLIC")

	dbExecute(t, dsn, `
CREATE FUNCTION test_schema.test() RETURNS text
	AS $$ select 'foo'::text $$
    LANGUAGE SQL;
`)
	defer func() {
		dbExecute(t, dsn, "DROP SCHEMA test_schema CASCADE")
		dbExecute(t, dsn, "DROP ROLE test_role")
	}()

	for _, role := range []string{"test_role", "public"} {
		t.Run(role, func(t *testing.T) {
			tfConfig := fmt.Sprintf(`
resource postgresql_grant "test" {
  database    = "postgres"
  role        = "%s"
  schema      = "test_schema"
  object_type = "function"
  privileges  = ["EXECUTE"]
}
	`, role)

			resource.Test(t, resource.TestCase{
				PreCheck: func() {
					testAccPreCheck(t)
					testCheckCompatibleVersion(t, featurePrivileges)
				},
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{
					{
						Config: tfConfig,
						Check: resource.ComposeTestCheckFunc(
							resource.TestCheckResourceAttr("postgresql_grant.test", "id", fmt.Sprintf("%s_postgres_test_schema_function", role)),
							resource.TestCheckResourceAttr("postgresql_grant.test", "privileges.#", "1"),
							resource.TestCheckResourceAttr("postgresql_grant.test", "privileges.0", "EXECUTE"),
							resource.TestCheckResourceAttr("postgresql_grant.test", "with_grant_option", "false"),
							grantFwCheckFunctionExecutable(t, "test_role", "test_schema.test"),
						),
					},
				},
			})
		})
	}
}
