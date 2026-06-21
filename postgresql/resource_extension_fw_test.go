package postgresql

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

func TestAccExtensionFW_Basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureExtension)
			// TODO: Need to check how RDS manage to allow `rds_superuser` to create extension
			// even it's not a real superuser
			testSuperuserPreCheck(t)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckPostgresqlExtensionDestroy,
		Steps: []resource.TestStep{
			{
				Config: `
				resource "postgresql_extension" "myextension" {
					name = "pg_trgm"
				}`,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlExtensionExists("postgresql_extension.myextension"),
					resource.TestCheckResourceAttr("postgresql_extension.myextension", "name", "pg_trgm"),
					resource.TestCheckResourceAttr("postgresql_extension.myextension", "schema", "public"),
					// NOTE: The version number drifts across Postgres releases.
					resource.TestCheckResourceAttrSet("postgresql_extension.myextension", "version"),
				),
			},
			{
				ResourceName:      "postgresql_extension.myextension",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccExtensionFW_SchemaRename(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureExtension)
			testSuperuserPreCheck(t)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckPostgresqlExtensionDestroy,
		Steps: []resource.TestStep{
			{
				Config: `
				resource "postgresql_schema" "extfwfoo" {
					name = "foo"
				}

				resource "postgresql_extension" "extfwtrgm" {
					name   = "pg_trgm"
					schema = postgresql_schema.extfwfoo.name
				}`,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlExtensionExists("postgresql_extension.extfwtrgm"),
					resource.TestCheckResourceAttr("postgresql_schema.extfwfoo", "name", "foo"),
					resource.TestCheckResourceAttr("postgresql_extension.extfwtrgm", "name", "pg_trgm"),
					resource.TestCheckResourceAttr("postgresql_extension.extfwtrgm", "schema", "foo"),
				),
			},
			{
				Config: `
				resource "postgresql_schema" "extfwfoo" {
					name = "bar"
				}

				resource "postgresql_extension" "extfwtrgm" {
					name   = "pg_trgm"
					schema = postgresql_schema.extfwfoo.name
				}`,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlExtensionExists("postgresql_extension.extfwtrgm"),
					resource.TestCheckResourceAttr("postgresql_schema.extfwfoo", "name", "bar"),
					resource.TestCheckResourceAttr("postgresql_extension.extfwtrgm", "name", "pg_trgm"),
					resource.TestCheckResourceAttr("postgresql_extension.extfwtrgm", "schema", "bar"),
				),
			},
		},
	})
}

func TestAccExtensionFW_Database(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	dbName, _ := getTestDBNames(dbSuffix)

	config := fmt.Sprintf(`
	resource "postgresql_extension" "myextension" {
		name     = "pg_trgm"
		database = "%s"
	}
	`, dbName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureExtension)
			testSuperuserPreCheck(t)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckPostgresqlExtensionDestroy,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlExtensionExists("postgresql_extension.myextension"),
					resource.TestCheckResourceAttr("postgresql_extension.myextension", "name", "pg_trgm"),
					resource.TestCheckResourceAttr("postgresql_extension.myextension", "schema", "public"),
					resource.TestCheckResourceAttr("postgresql_extension.myextension", "database", dbName),
					resource.TestCheckResourceAttrSet("postgresql_extension.myextension", "version"),
				),
			},
		},
	})
}

func TestAccExtensionFW_DropCascade(t *testing.T) {
	skipIfNotAcc(t)

	config := `
	resource "postgresql_extension" "cascade" {
		name         = "pgcrypto"
		drop_cascade = true
	}
	`
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureExtension)
			testSuperuserPreCheck(t)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckPostgresqlExtensionDestroy,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlExtensionExists("postgresql_extension.cascade"),
					resource.TestCheckResourceAttr("postgresql_extension.cascade", "name", "pgcrypto"),
					resource.TestCheckResourceAttr("postgresql_extension.cascade", "drop_cascade", "true"),
					// This will create a dependency on the extension.
					testAccCreateExtensionDependency("test_extension_cascade_fw"),
				),
			},
		},
	})
}

func TestAccExtensionFW_CreateCascade(t *testing.T) {
	skipIfNotAcc(t)

	config := `
	resource "postgresql_extension" "cascade" {
		name           = "earthdistance"
		create_cascade = true
	}
	`
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureExtension)
			testSuperuserPreCheck(t)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckPostgresqlExtensionDestroy,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlExtensionExists("postgresql_extension.cascade"),
					resource.TestCheckResourceAttr("postgresql_extension.cascade", "name", "earthdistance"),
					resource.TestCheckResourceAttr("postgresql_extension.cascade", "create_cascade", "true"),
					// The cube extension should be installed as a dependency of earthdistance.
					testAccCheckExtensionDependency("cube"),
				),
			},
		},
	})
}

func testAccCheckPostgresqlExtensionDestroy(s *terraform.State) error {
	client := testAccProvider.Meta().(*Client)

	for _, rs := range s.RootModule().Resources {
		if rs.Type != "postgresql_extension" {
			continue
		}

		database, ok := rs.Primary.Attributes["database"]
		if !ok {
			return fmt.Errorf("No Attribute for database is set")
		}
		txn, err := startTransaction(client, database)
		if err != nil {
			return err
		}
		defer deferredRollback(txn)

		exists, err := checkExtensionExists(txn, getExtensionNameFromID(rs.Primary.ID))

		if err != nil {
			return fmt.Errorf("error checking extension %s", err)
		}

		if exists {
			return fmt.Errorf("Extension still exists after destroy")
		}
	}

	return nil
}

func testAccCheckPostgresqlExtensionExists(n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("Resource not found: %s", n)
		}

		if rs.Primary.ID == "" {
			return fmt.Errorf("No ID is set")
		}

		database, ok := rs.Primary.Attributes["database"]
		if !ok {
			return fmt.Errorf("No Attribute for database is set")
		}

		extName, ok := rs.Primary.Attributes["name"]
		if !ok {
			return fmt.Errorf("No Attribute for extension name is set")
		}

		client := testAccProvider.Meta().(*Client)
		txn, err := startTransaction(client, database)
		if err != nil {
			return err
		}
		defer deferredRollback(txn)

		exists, err := checkExtensionExists(txn, extName)

		if err != nil {
			return fmt.Errorf("error checking extension %s", err)
		}

		if !exists {
			return fmt.Errorf("Extension not found")
		}

		return nil
	}
}

func checkExtensionExists(txn *sql.Tx, extensionName string) (bool, error) {
	var _rez bool
	err := txn.QueryRow("SELECT TRUE from pg_catalog.pg_extension d WHERE extname=$1", extensionName).Scan(&_rez)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("error reading info about extension: %s", err)
	}

	return true, nil
}

func getExtensionNameFromID(ID string) string {
	splitted := strings.Split(ID, ".")
	return splitted[0]
}

func testAccCreateExtensionDependency(tableName string) resource.TestCheckFunc {
	return func(s *terraform.State) error {

		client := testAccProvider.Meta().(*Client)
		db, err := client.Connect()
		if err != nil {
			return err
		}

		_, err = db.Exec(fmt.Sprintf("DROP TABLE IF EXISTS %s; CREATE TABLE %s (id uuid DEFAULT gen_random_uuid())", tableName, tableName))
		if err != nil {
			return fmt.Errorf("could not create test table in schema: %s", err)
		}

		return nil
	}
}

func testAccCheckExtensionDependency(extName string) resource.TestCheckFunc {
	return func(s *terraform.State) error {

		client := testAccProvider.Meta().(*Client)
		txn, err := startTransaction(client, client.databaseName)
		if err != nil {
			return err
		}
		defer deferredRollback(txn)

		exists, err := checkExtensionExists(txn, extName)
		if err != nil {
			return fmt.Errorf("error checking extension %s", err)
		}
		if !exists {
			return fmt.Errorf("Extension %s not found", extName)
		}

		return nil
	}
}
