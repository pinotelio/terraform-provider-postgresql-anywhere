package postgresql

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

func TestAccUserMapping_Basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureServer)
			testSuperuserPreCheck(t)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckUserMappingDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccUserMappingConfigFW,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckUserMappingExists("postgresql_user_mapping.remote"),
					resource.TestCheckResourceAttr(
						"postgresql_user_mapping.remote", "server_name", "myserver_postgres"),
					resource.TestCheckResourceAttr(
						"postgresql_user_mapping.remote", "user_name", "remote"),
					resource.TestCheckResourceAttr(
						"postgresql_user_mapping.remote", "options.user", "admin"),
					resource.TestCheckResourceAttr(
						"postgresql_user_mapping.remote", "options.password", "pass"),
					resource.TestCheckResourceAttr(
						"postgresql_user_mapping.special_chars", "options.password", "pass=$*'"),
				),
			},
			{
				ResourceName:      "postgresql_user_mapping.remote",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccUserMapping_Update(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureServer)
			testSuperuserPreCheck(t)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckUserMappingDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccUserMappingConfigFW,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckUserMappingExists("postgresql_user_mapping.remote"),
					resource.TestCheckResourceAttr(
						"postgresql_user_mapping.remote", "server_name", "myserver_postgres"),
					resource.TestCheckResourceAttr(
						"postgresql_user_mapping.remote", "options.password", "pass"),
				),
			},
			{
				Config: testAccUserMappingChanges2FW,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckUserMappingExists("postgresql_user_mapping.remote"),
					resource.TestCheckResourceAttr(
						"postgresql_user_mapping.remote", "options.password", "passUpdated"),
				),
			},
			{
				Config: testAccUserMappingChanges3FW,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckUserMappingExists("postgresql_user_mapping.remote"),
					resource.TestCheckNoResourceAttr(
						"postgresql_user_mapping.remote", "options.user"),
					resource.TestCheckNoResourceAttr(
						"postgresql_user_mapping.remote", "options.password"),
				),
			},
		},
	})
}

func checkUserMappingExistsFW(txn *sql.Tx, username string, serverName string) (bool, error) {
	var rez bool
	err := txn.QueryRow("SELECT TRUE FROM pg_user_mappings WHERE usename = $1 AND srvname = $2", username, serverName).Scan(&rez)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("error reading info about user mapping: %w", err)
	}
	return true, nil
}

func testAccCheckUserMappingExists(n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("resource not found: %s", n)
		}
		if rs.Primary.ID == "" {
			return fmt.Errorf("no ID is set")
		}

		username, ok := rs.Primary.Attributes["user_name"]
		if !ok {
			return fmt.Errorf("no attribute for user_name is set")
		}
		serverName, ok := rs.Primary.Attributes["server_name"]
		if !ok {
			return fmt.Errorf("no attribute for server_name is set")
		}

		client := testAccProvider.Meta().(*Client)
		txn, err := startTransaction(client, "")
		if err != nil {
			return err
		}
		defer deferredRollback(txn)

		exists, err := checkUserMappingExistsFW(txn, username, serverName)
		if err != nil {
			return fmt.Errorf("error checking user mapping: %w", err)
		}
		if !exists {
			return fmt.Errorf("user mapping not found")
		}
		return nil
	}
}

func testAccCheckUserMappingDestroy(s *terraform.State) error {
	client := testAccProvider.Meta().(*Client)

	for _, rs := range s.RootModule().Resources {
		if rs.Type != "postgresql_user_mapping" {
			continue
		}

		txn, err := startTransaction(client, "")
		if err != nil {
			return err
		}
		defer deferredRollback(txn)

		splitted := strings.SplitN(rs.Primary.ID, ".", 2)
		exists, err := checkUserMappingExistsFW(txn, splitted[0], splitted[1])
		if err != nil {
			return fmt.Errorf("error checking user mapping: %w", err)
		}
		if exists {
			return fmt.Errorf("user mapping still exists after destroy")
		}
	}

	return nil
}

var testAccUserMappingConfigFW = `
resource "postgresql_extension" "ext_postgres_fdw" {
  name = "postgres_fdw"
}

resource "postgresql_server" "myserver_postgres" {
  server_name = "myserver_postgres"
  fdw_name    = "postgres_fdw"
  options = {
    host   = "foo"
    dbname = "foodb"
    port   = "5432"
  }

  depends_on = [postgresql_extension.ext_postgres_fdw]
}

resource "postgresql_role" "remote" {
  name = "remote"
}

resource "postgresql_user_mapping" "remote" {
  server_name = postgresql_server.myserver_postgres.server_name
  user_name   = postgresql_role.remote.name
  options = {
    user = "admin"
    password = "pass"
  }
}

resource "postgresql_role" "special" {
  name = "special"
}

resource "postgresql_user_mapping" "special_chars" {
  server_name = postgresql_server.myserver_postgres.server_name
  user_name   = postgresql_role.special.name
  options = {
	user = "admin"
	password = "pass=$*'"
  }
}
`

var testAccUserMappingChanges2FW = `
resource "postgresql_extension" "ext_postgres_fdw" {
	name = "postgres_fdw"
  }

  resource "postgresql_server" "myserver_postgres" {
	server_name = "myserver_postgres"
	fdw_name    = "postgres_fdw"
	options = {
	  host   = "foo"
	  dbname = "foodb"
	  port   = "5432"
	}

	depends_on = [postgresql_extension.ext_postgres_fdw]
  }

  resource "postgresql_role" "remote" {
	name = "remote"
  }

  resource "postgresql_user_mapping" "remote" {
	server_name = postgresql_server.myserver_postgres.server_name
	user_name   = postgresql_role.remote.name
	options = {
	  user = "admin"
	  password = "passUpdated"
	}
  }
`

var testAccUserMappingChanges3FW = `
resource "postgresql_extension" "ext_postgres_fdw" {
	name = "postgres_fdw"
  }

  resource "postgresql_server" "myserver_postgres" {
	server_name = "myserver_postgres"
	fdw_name    = "postgres_fdw"
	options = {
	  host   = "foo"
	  dbname = "foodb"
	  port   = "5432"
	}

	depends_on = [postgresql_extension.ext_postgres_fdw]
  }

  resource "postgresql_role" "remote" {
	name = "remote"
  }

  resource "postgresql_user_mapping" "remote" {
	server_name = postgresql_server.myserver_postgres.server_name
	user_name   = postgresql_role.remote.name
  }
`
