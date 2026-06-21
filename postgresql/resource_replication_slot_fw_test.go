package postgresql

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

func TestAccReplicationSlot_Basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testSuperuserPreCheck(t)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckReplicationSlotDestroy,
		Steps: []resource.TestStep{
			{
				Config: `
				resource "postgresql_replication_slot" "myslot" {
					name   = "slot"
					plugin = "test_decoding"
				}`,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckReplicationSlotExists("postgresql_replication_slot.myslot"),
					resource.TestCheckResourceAttr("postgresql_replication_slot.myslot", "name", "slot"),
					resource.TestCheckResourceAttr("postgresql_replication_slot.myslot", "plugin", "test_decoding"),
				),
			},
			{
				ResourceName:      "postgresql_replication_slot.myslot",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccReplicationSlot_Database(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	dbName, _ := getTestDBNames(dbSuffix)

	config := fmt.Sprintf(`
	resource "postgresql_replication_slot" "myslot" {
		name     = "slot"
		plugin   = "test_decoding"
		database = "%s"
	}`, dbName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testSuperuserPreCheck(t)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckReplicationSlotDestroy,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckReplicationSlotExists("postgresql_replication_slot.myslot"),
					resource.TestCheckResourceAttr("postgresql_replication_slot.myslot", "name", "slot"),
					resource.TestCheckResourceAttr("postgresql_replication_slot.myslot", "plugin", "test_decoding"),
					resource.TestCheckResourceAttr("postgresql_replication_slot.myslot", "database", dbName),
				),
			},
		},
	})
}

func testAccCheckReplicationSlotExists(n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("resource not found: %s", n)
		}
		if rs.Primary.ID == "" {
			return fmt.Errorf("no ID is set")
		}
		database := rs.Primary.Attributes["database"]
		name := rs.Primary.Attributes["name"]

		client := testAccProvider.Meta().(*Client)
		txn, err := startTransaction(client, database)
		if err != nil {
			return err
		}
		defer deferredRollback(txn)

		exists, err := checkReplicationSlotExistsFW(txn, name)
		if err != nil {
			return fmt.Errorf("error checking replication slot: %w", err)
		}
		if !exists {
			return fmt.Errorf("replication slot not found")
		}
		return nil
	}
}

func testAccCheckReplicationSlotDestroy(s *terraform.State) error {
	client := testAccProvider.Meta().(*Client)

	for _, rs := range s.RootModule().Resources {
		if rs.Type != "postgresql_replication_slot" {
			continue
		}
		database := rs.Primary.Attributes["database"]
		txn, err := startTransaction(client, database)
		if err != nil {
			return err
		}
		defer deferredRollback(txn)

		exists, err := checkReplicationSlotExistsFW(txn, rs.Primary.Attributes["name"])
		if err != nil {
			return fmt.Errorf("error checking replication slot: %w", err)
		}
		if exists {
			return fmt.Errorf("replication slot still exists after destroy")
		}
	}
	return nil
}

func checkReplicationSlotExistsFW(txn *sql.Tx, slotName string) (bool, error) {
	var rez bool
	err := txn.QueryRow("SELECT TRUE FROM pg_catalog.pg_replication_slots WHERE slot_name=$1", slotName).Scan(&rez)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("error reading replication slot: %w", err)
	}
	return true, nil
}
