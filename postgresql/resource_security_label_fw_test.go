package postgresql

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

func TestAccSecurityLabel_Basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureSecurityLabel)
			testSuperuserPreCheck(t)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckSecurityLabelDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccSecurityLabelConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckSecurityLabelExists("postgresql_security_label.test_label"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "object_type", "role"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "object_name", "security_label_test_role"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "label_provider", "dummy"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "label", "secret"),
				),
			},
			{
				ResourceName:      "postgresql_security_label.test_label",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccSecurityLabel_Update(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureSecurityLabel)
			testSuperuserPreCheck(t)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckSecurityLabelDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccSecurityLabelConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckSecurityLabelExists("postgresql_security_label.test_label"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "object_type", "role"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "object_name", "security_label_test_role"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "label_provider", "dummy"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "label", "secret"),
				),
			},
			{
				Config: testAccSecurityLabelChanges2,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckSecurityLabelExists("postgresql_security_label.test_label"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "label", "top secret"),
				),
			},
			{
				Config: testAccSecurityLabelChanges3,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckSecurityLabelExists("postgresql_security_label.test_label"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "object_name", "security_label_test-role2"),
				),
			},
		},
	})
}

func securityLabelExistsFW(txn *sql.Tx, objectType string, objectName string, provider string) (bool, error) {
	var _rez bool
	err := txn.QueryRow(
		"SELECT TRUE FROM pg_seclabels WHERE objtype = $1 AND objname = $2 AND provider = $3",
		objectType, securityLabelQuoteIdentifier(objectName), provider,
	).Scan(&_rez)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("error reading info about security label: %s", err)
	}

	return true, nil
}

func testAccCheckSecurityLabelDestroy(s *terraform.State) error {
	client := testAccProvider.Meta().(*Client)

	for _, rs := range s.RootModule().Resources {
		if rs.Type != "postgresql_security_label" {
			continue
		}

		txn, err := startTransaction(client, "")
		if err != nil {
			return err
		}
		defer deferredRollback(txn)

		splitted := strings.SplitN(rs.Primary.ID, ".", 3)
		exists, err := securityLabelExistsFW(txn, splitted[1], splitted[2], splitted[0])
		if err != nil {
			return fmt.Errorf("error checking security label%s", err)
		}

		if exists {
			return fmt.Errorf("Security label still exists after destroy")
		}
	}

	return nil
}

func testAccCheckSecurityLabelExists(n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("Resource not found: %s", n)
		}

		if rs.Primary.ID == "" {
			return fmt.Errorf("No ID is set")
		}

		objectType, ok := rs.Primary.Attributes[securityLabelObjectTypeAttr]
		if !ok {
			return fmt.Errorf("No Attribute for object type is set")
		}

		objectName, ok := rs.Primary.Attributes[securityLabelObjectNameAttr]
		if !ok {
			return fmt.Errorf("No Attribute for object name is set")
		}

		provider, ok := rs.Primary.Attributes[securityLabelProviderAttr]
		if !ok {
			return fmt.Errorf("No Attribute for security provider is set")
		}

		client := testAccProvider.Meta().(*Client)
		txn, err := startTransaction(client, "")
		if err != nil {
			return err
		}
		defer deferredRollback(txn)

		exists, err := securityLabelExistsFW(txn, objectType, objectName, provider)
		if err != nil {
			return fmt.Errorf("error checking security label%s", err)
		}

		if !exists {
			return fmt.Errorf("Security label not found")
		}

		return nil
	}
}

var testAccSecurityLabelConfig = `
resource "postgresql_role" "test_role" {
  name            = "security_label_test_role"
  login           = true
  create_database = true
}
resource "postgresql_security_label" "test_label" {
  object_type    = "role"
  object_name    = postgresql_role.test_role.name
  label_provider = "dummy"
  label          = "secret"
}
`

var testAccSecurityLabelChanges2 = `
resource "postgresql_role" "test_role" {
  name            = "security_label_test_role"
  login           = true
  create_database = true
}
resource "postgresql_security_label" "test_label" {
  object_type    = "role"
  object_name    = postgresql_role.test_role.name
  label_provider = "dummy"
  label          = "top secret"
}
`

var testAccSecurityLabelChanges3 = `
resource "postgresql_role" "test_role" {
  name            = "security_label_test-role2"
  login           = true
  create_database = true
}
resource "postgresql_security_label" "test_label" {
  object_type    = "role"
  object_name    = postgresql_role.test_role.name
  label_provider = "dummy"
  label          = "top secret"
}
`
