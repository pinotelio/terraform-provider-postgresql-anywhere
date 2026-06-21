package postgresql

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

func TestAccGrantRole_Basic(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, false, true)
	defer teardown()

	_, roleName := getTestDBNames(dbSuffix)

	grantedRoleName := "foo"

	config := fmt.Sprintf(`
	resource postgresql_role "grant" {
		name = "%s"
	}
	resource postgresql_grant_role "grant_role" {
		role              = "%s"
		grant_role        = postgresql_role.grant.name
		with_admin_option = true
	}
	`, grantedRoleName, roleName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckGrantRoleDestroy,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckGrantRoleExists("postgresql_grant_role.grant_role"),
					resource.TestCheckResourceAttr(
						"postgresql_grant_role.grant_role", "role", roleName),
					resource.TestCheckResourceAttr(
						"postgresql_grant_role.grant_role", "grant_role", grantedRoleName),
					resource.TestCheckResourceAttr(
						"postgresql_grant_role.grant_role", "with_admin_option", strconv.FormatBool(true)),
					resource.TestCheckResourceAttr(
						"postgresql_grant_role.grant_role", "id",
						grantRoleGenerateID(roleName, grantedRoleName, true)),
				),
			},
		},
	})
}

func testAccCheckGrantRoleExists(n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("resource not found: %s", n)
		}
		if rs.Primary.ID == "" {
			return fmt.Errorf("no ID is set")
		}

		role := rs.Primary.Attributes["role"]
		grantRole := rs.Primary.Attributes["grant_role"]

		client := testAccProvider.Meta().(*Client)
		txn, err := startTransaction(client, "")
		if err != nil {
			return err
		}
		defer deferredRollback(txn)

		_, _, _, found, err := grantRoleReadMembership(txn, role, grantRole)
		if err != nil {
			return fmt.Errorf("error checking grant role: %w", err)
		}
		if !found {
			return fmt.Errorf("role %s is not a member of %s", role, grantRole)
		}
		return nil
	}
}

func testAccCheckGrantRoleDestroy(s *terraform.State) error {
	client := testAccProvider.Meta().(*Client)

	for _, rs := range s.RootModule().Resources {
		if rs.Type != "postgresql_grant_role" {
			continue
		}

		role := rs.Primary.Attributes["role"]
		grantRole := rs.Primary.Attributes["grant_role"]

		txn, err := startTransaction(client, "")
		if err != nil {
			return err
		}
		defer deferredRollback(txn)

		_, _, _, found, err := grantRoleReadMembership(txn, role, grantRole)
		if err != nil {
			return fmt.Errorf("error checking grant role: %w", err)
		}
		if found {
			return fmt.Errorf("grant role still exists after destroy")
		}
	}
	return nil
}
