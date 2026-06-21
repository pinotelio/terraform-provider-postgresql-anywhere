package postgresql

import (
	"database/sql"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

// checkPublicationExistsFW reports whether a publication exists.
func checkPublicationExistsFW(txn *sql.Tx, pubName string) (bool, error) {
	var rez bool
	err := txn.QueryRow("SELECT TRUE from pg_catalog.pg_publication WHERE pubname=$1", pubName).Scan(&rez)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("error reading info about publication: %s", err)
	}
	return true, nil
}

func testAccCheckPublicationExistsFW(n string) resource.TestCheckFunc {
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

		pubName, ok := rs.Primary.Attributes["name"]
		if !ok {
			return fmt.Errorf("No Attribute for publication name is set")
		}

		client := testAccProvider.Meta().(*Client)
		txn, err := startTransaction(client, database)
		if err != nil {
			return err
		}
		defer deferredRollback(txn)

		exists, err := checkPublicationExistsFW(txn, pubName)
		if err != nil {
			return fmt.Errorf("error checking publication %s", err)
		}
		if !exists {
			return fmt.Errorf("Publication not found")
		}
		return nil
	}
}

func testAccCheckPublicationDestroyFW(s *terraform.State) error {
	client := testAccProvider.Meta().(*Client)

	for _, rs := range s.RootModule().Resources {
		if rs.Type != "postgresql_publication" {
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

		exists, err := checkPublicationExistsFW(txn, rs.Primary.Attributes["name"])
		if err != nil {
			return fmt.Errorf("error checking publication %s", err)
		}
		if exists {
			return fmt.Errorf("Publication still exists after destroy")
		}
	}

	return nil
}

func TestAccPublicationFW_Database(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	dbName, _ := getTestDBNames(dbSuffix)

	config := fmt.Sprintf(`
	resource "postgresql_role" "test" {
		name = "test"
	}
	resource "postgresql_publication" "test" {
		name     = "publication"
		database = "%s"
		owner = postgresql_role.test.name
	}
	`, dbName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePublication)
			testSuperuserPreCheck(t)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckPublicationDestroyFW,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPublicationExistsFW("postgresql_publication.test"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "name", "publication"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "owner", "test"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "database", dbName),
				),
			},
			{
				ResourceName:            "postgresql_publication.test",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"drop_cascade"},
			},
		},
	})
}

func TestAccPublicationFW_UpdateTables(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()
	testTables := []string{"test_schema.test_table_1", "test_schema.test_table_2", "test_schema.test_table_3"}
	createTestTables(t, dbSuffix, testTables, "")

	dbName, _ := getTestDBNames(dbSuffix)

	baseConfig := fmt.Sprintf(`
	resource "postgresql_publication" "test" {
		name     = "publication"
		database = "%s"
		tables = ["test_schema.test_table_1", "test_schema.test_table_2"]
	}
	`, dbName)

	updateConfig := fmt.Sprintf(`
	resource "postgresql_publication" "test" {
		name     = "publication"
		database = "%s"
		tables = ["test_schema.test_table_1", "test_schema.test_table_3"]
	}
	`, dbName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePublication)
			testSuperuserPreCheck(t)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckPublicationDestroyFW,
		Steps: []resource.TestStep{
			{
				Config:  baseConfig,
				Destroy: false,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPublicationExistsFW("postgresql_publication.test"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "name", "publication"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "database", dbName),
					resource.TestCheckResourceAttr("postgresql_publication.test", "all_tables", "false"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "tables.0", "test_schema.test_table_1"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "tables.1", "test_schema.test_table_2"),
				),
			},
			{
				Config: updateConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPublicationExistsFW("postgresql_publication.test"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "name", "publication"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "database", dbName),
					resource.TestCheckResourceAttr("postgresql_publication.test", "all_tables", "false"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "tables.0", "test_schema.test_table_1"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "tables.1", "test_schema.test_table_3"),
				),
			},
		},
	})
}

func TestAccPublicationFW_UpdatePublishParams(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()
	testTables := []string{"test_schema.test_table_1", "test_schema.test_table_2", "test_schema.test_table_3"}
	createTestTables(t, dbSuffix, testTables, "")

	dbName, _ := getTestDBNames(dbSuffix)

	baseConfig := fmt.Sprintf(`
	resource "postgresql_publication" "test" {
		name     = "publication"
		database = "%s"
	}
	`, dbName)

	updateConfig := fmt.Sprintf(`
	resource "postgresql_publication" "test" {
		name     = "publication"
		database = "%s"
		publish_param = ["update", "truncate"]
	}
	`, dbName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePublication)
			testCheckCompatibleVersion(t, featurePubTruncate)
			testSuperuserPreCheck(t)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckPublicationDestroyFW,
		Steps: []resource.TestStep{
			{
				Config:  baseConfig,
				Destroy: false,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPublicationExistsFW("postgresql_publication.test"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "name", "publication"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "database", dbName),
					resource.TestCheckResourceAttr("postgresql_publication.test", "publish_param.#", "4"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "publish_param.0", "insert"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "publish_param.1", "update"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "publish_param.2", "delete"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "publish_param.3", "truncate"),
				),
			},
			{
				Config: updateConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPublicationExistsFW("postgresql_publication.test"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "name", "publication"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "database", dbName),
					resource.TestCheckResourceAttr("postgresql_publication.test", "publish_param.#", "2"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "publish_param.0", "update"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "publish_param.1", "truncate"),
				),
			},
		},
	})
}

func TestAccPublicationFW_UpdateOwner(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	dbName, _ := getTestDBNames(dbSuffix)
	testOwner := "test_owner"

	baseConfig := fmt.Sprintf(`
	resource "postgresql_publication" "test" {
		name     = "publication"
		database = "%s"
	}
	`, dbName)

	updateConfig := fmt.Sprintf(`
	resource "postgresql_role" "test_owner_2" {
		name = "%s_2"
		login = true
	}
	resource "postgresql_publication" "test" {
		name     = "publication"
		database = "%s"
		owner = "${postgresql_role.test_owner_2.name}"
	}
	`, testOwner, dbName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePublication)
			testSuperuserPreCheck(t)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckPublicationDestroyFW,
		Steps: []resource.TestStep{
			{
				Config:  baseConfig,
				Destroy: false,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPublicationExistsFW("postgresql_publication.test"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "owner", "postgres"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "name", "publication"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "database", dbName),
				),
			},
			{
				Config: updateConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPublicationExistsFW("postgresql_publication.test"),
					resource.TestCheckResourceAttr("postgresql_role.test_owner_2", "name", fmt.Sprintf("%s_2", testOwner)),
					resource.TestCheckResourceAttr("postgresql_publication.test", "name", "publication"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "owner", fmt.Sprintf("%s_2", testOwner)),
					resource.TestCheckResourceAttr("postgresql_publication.test", "database", dbName),
				),
			},
		},
	})
}

func TestAccPublicationFW_UpdateName(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	dbName, _ := getTestDBNames(dbSuffix)

	baseConfig := fmt.Sprintf(`
	resource "postgresql_publication" "test" {
		name     = "%s_publication_1"
		database = "%s"
	}
	`, dbName, dbName)

	updateConfig := fmt.Sprintf(`
	resource "postgresql_publication" "test" {
		name     = "%s_publication_2"
		database = "%s"
	}
	`, dbName, dbName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePublication)
			testSuperuserPreCheck(t)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckPublicationDestroyFW,
		Steps: []resource.TestStep{
			{
				Config: baseConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPublicationExistsFW("postgresql_publication.test"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "name", fmt.Sprintf("%s_publication_1", dbName)),
					resource.TestCheckResourceAttr("postgresql_publication.test", "database", dbName),
				),
			},
			{
				Config: updateConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPublicationExistsFW("postgresql_publication.test"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "name", fmt.Sprintf("%s_publication_2", dbName)),
					resource.TestCheckResourceAttr("postgresql_publication.test", "database", dbName),
				),
			},
			{
				Config: baseConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPublicationExistsFW("postgresql_publication.test"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "name", fmt.Sprintf("%s_publication_1", dbName)),
					resource.TestCheckResourceAttr("postgresql_publication.test", "database", dbName),
				),
			},
		},
	})
}

func TestAccPublicationFW_Basic(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()
	testTables := []string{"test_schema.test_table_1", "test_schema.test_table_2", "test_schema.test_table_3"}
	createTestTables(t, dbSuffix, testTables, "")

	dbName, _ := getTestDBNames(dbSuffix)
	config := fmt.Sprintf(`
resource "postgresql_role" "test_owner" {
	name = "test_owner"
	login = true
}

resource "postgresql_publication" "test" {
	name     = "publication"
	database = "%s"
	owner = "${postgresql_role.test_owner.name}"
	all_tables = true
}
`, dbName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePublication)
			testCheckCompatibleVersion(t, featurePubTruncate)
			testSuperuserPreCheck(t)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckPublicationDestroyFW,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPublicationExistsFW("postgresql_publication.test"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "name", "publication"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "database", dbName),
					resource.TestCheckResourceAttr("postgresql_publication.test", "all_tables", "true"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "owner", "test_owner"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "tables.#", "3"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "publish_param.#", "4"),
				),
			},
		},
	})
}

func TestAccPublicationFW_ConflictTables(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	dbName, _ := getTestDBNames(dbSuffix)
	config := fmt.Sprintf(`
resource "postgresql_publication" "test" {
	name     = "publication"
	database = "%s"
	tables = ["test.table1","test.table2"]
	all_tables = true
}
`, dbName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePublication)
			testSuperuserPreCheck(t)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckPublicationDestroyFW,
		Steps: []resource.TestStep{
			{
				Config:      config,
				ExpectError: regexp.MustCompile("Invalid Attribute Combination"),
			},
		},
	})
}

func TestAccPublicationFW_CheckPublishViaRoot(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	dbName, _ := getTestDBNames(dbSuffix)
	baseConfig := fmt.Sprintf(`
resource "postgresql_publication" "test" {
	name     = "publication"
	database = "%s"
}
`, dbName)

	updateConfig := fmt.Sprintf(`
resource "postgresql_publication" "test" {
	name     = "publication"
	database = "%s"
	publish_param = ["update","delete"]
	publish_via_partition_root_param = true
}
`, dbName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePublication)
			testCheckCompatibleVersion(t, featurePublishViaRoot)
			testSuperuserPreCheck(t)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckPublicationDestroyFW,
		Steps: []resource.TestStep{
			{
				Config: baseConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPublicationExistsFW("postgresql_publication.test"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "name", "publication"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "database", dbName),
					resource.TestCheckResourceAttr("postgresql_publication.test", "publish_via_partition_root_param", "false"),
				),
			},
			{
				Config: updateConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPublicationExistsFW("postgresql_publication.test"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "name", "publication"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "database", dbName),
					resource.TestCheckResourceAttr("postgresql_publication.test", "publish_param.#", "2"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "publish_param.0", "update"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "publish_param.1", "delete"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "publish_via_partition_root_param", "true"),
				),
			},
		},
	})
}

func TestAccPublicationFW_CheckPublishParams(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	dbName, _ := getTestDBNames(dbSuffix)
	baseConfig := fmt.Sprintf(`
resource "postgresql_publication" "test" {
	name     = "publication"
	database = "%s"
	publish_param = ["insert"]
}
`, dbName)
	wrongKeys := fmt.Sprintf(`
resource "postgresql_publication" "test" {
	name     = "publication"
	database = "%s"
	publish_param = ["insert","wrong_param"]
}
`, dbName)
	duplicateKeys := fmt.Sprintf(`
resource "postgresql_publication" "test" {
	name     = "publication"
	database = "%s"
	publish_param = ["insert","insert"]
}
`, dbName)
	updateKeys := fmt.Sprintf(`
resource "postgresql_publication" "test" {
	name     = "publication"
	database = "%s"
	publish_param = ["update","delete"]
}
`, dbName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePublication)
			testSuperuserPreCheck(t)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckPublicationDestroyFW,
		Steps: []resource.TestStep{
			{
				Config: baseConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPublicationExistsFW("postgresql_publication.test"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "name", "publication"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "database", dbName),
					resource.TestCheckResourceAttr("postgresql_publication.test", "publish_param.#", "1"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "publish_param.0", "insert"),
				),
			},
			{
				Config: updateKeys,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPublicationExistsFW("postgresql_publication.test"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "name", "publication"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "database", dbName),
					resource.TestCheckResourceAttr("postgresql_publication.test", "publish_param.#", "2"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "publish_param.0", "update"),
					resource.TestCheckResourceAttr("postgresql_publication.test", "publish_param.1", "delete"),
				),
			},
			{
				// Terraform wraps long diagnostic detail across lines, so match a
				// short single-token substring of the validation error.
				Config:      wrongKeys,
				ExpectError: regexp.MustCompile("wrong_param"),
			},
			{
				Config:      duplicateKeys,
				ExpectError: regexp.MustCompile("duplicated"),
			},
		},
	})
}
