package postgresql

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

func TestAccPhysicalReplicationSlot_Basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testSuperuserPreCheck(t)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckPhysicalReplicationSlotDestroy,
		Steps: []resource.TestStep{
			{
				Config: `
				resource "postgresql_physical_replication_slot" "myslot" {
					name = "physical_slot"
				}`,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPhysicalReplicationSlotExists("postgresql_physical_replication_slot.myslot"),
					resource.TestCheckResourceAttr("postgresql_physical_replication_slot.myslot", "name", "physical_slot"),
					resource.TestCheckResourceAttr("postgresql_physical_replication_slot.myslot", "id", "physical_slot"),
				),
			},
			{
				ResourceName:      "postgresql_physical_replication_slot.myslot",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func testAccCheckPhysicalReplicationSlotExists(n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("resource not found: %s", n)
		}
		if rs.Primary.ID == "" {
			return fmt.Errorf("no ID is set")
		}
		name := rs.Primary.Attributes["name"]

		client := testAccProvider.Meta().(*Client)
		txn, err := startTransaction(client, client.DatabaseName())
		if err != nil {
			return err
		}
		defer deferredRollback(txn)

		exists, err := checkPhysicalReplicationSlotExistsFW(txn, name)
		if err != nil {
			return fmt.Errorf("error checking physical replication slot: %w", err)
		}
		if !exists {
			return fmt.Errorf("physical replication slot not found")
		}
		return nil
	}
}

func testAccCheckPhysicalReplicationSlotDestroy(s *terraform.State) error {
	client := testAccProvider.Meta().(*Client)

	for _, rs := range s.RootModule().Resources {
		if rs.Type != "postgresql_physical_replication_slot" {
			continue
		}
		txn, err := startTransaction(client, client.DatabaseName())
		if err != nil {
			return err
		}
		defer deferredRollback(txn)

		exists, err := checkPhysicalReplicationSlotExistsFW(txn, rs.Primary.Attributes["name"])
		if err != nil {
			return fmt.Errorf("error checking physical replication slot: %w", err)
		}
		if exists {
			return fmt.Errorf("physical replication slot still exists after destroy")
		}
	}
	return nil
}

func checkPhysicalReplicationSlotExistsFW(txn *sql.Tx, slotName string) (bool, error) {
	var rez bool
	err := txn.QueryRow(
		"SELECT TRUE FROM pg_catalog.pg_replication_slots WHERE slot_name=$1 and slot_type='physical'",
		slotName,
	).Scan(&rez)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("error reading physical replication slot: %w", err)
	}
	return true, nil
}
