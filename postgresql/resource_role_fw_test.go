package postgresql

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

func TestAccRoleFW_Basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckRoleFWDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlRoleFWConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckRoleFWExists("myrole2", nil, nil),
					resource.TestCheckResourceAttr("postgresql_role.myrole2", "name", "myrole2"),
					resource.TestCheckResourceAttr("postgresql_role.myrole2", "login", "true"),
					resource.TestCheckResourceAttr("postgresql_role.myrole2", "roles.#", "0"),

					testAccCheckRoleFWExists("role_default", nil, nil),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "name", "role_default"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "superuser", "false"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "create_database", "false"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "create_role", "false"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "inherit", "false"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "replication", "false"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "bypass_row_level_security", "false"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "connection_limit", "-1"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "encrypted_password", "true"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "password", ""),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "valid_until", "infinity"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "skip_drop_role", "false"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "skip_reassign_owned", "false"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "statement_timeout", "0"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "idle_in_transaction_session_timeout", "0"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "assume_role", ""),

					resource.TestCheckResourceAttr("postgresql_role.role_with_create_database", "name", "role_with_create_database"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_create_database", "create_database", "true"),

					testAccCheckRoleFWExists("sub_role", []string{"myrole2", "role_simple"}, nil),
					resource.TestCheckResourceAttr("postgresql_role.sub_role", "name", "sub_role"),
					resource.TestCheckResourceAttr("postgresql_role.sub_role", "roles.#", "2"),

					testAccCheckRoleFWExists("role_with_search_path", nil, []string{"bar", "foo-with-hyphen"}),
				),
			},
		},
	})
}

// Test creating a superuser role.
func TestAccRoleFW_Superuser(t *testing.T) {

	roleConfig := `
resource "postgresql_role" "role_with_superuser" {
  name = "role_with_superuser"
  superuser = true
  login = true
  password = "mypass"
}`

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
			// Need to a be a superuser to create a superuser
			testSuperuserPreCheck(t)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckRoleFWDestroy,
		Steps: []resource.TestStep{
			{
				Config: roleConfig,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_role.role_with_superuser", "name", "role_with_superuser"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_superuser", "superuser", "true"),
				),
			},
		},
	})
}

func TestAccRoleFW_Update(t *testing.T) {

	var configCreate = `
resource "postgresql_role" "update_role" {
  name = "update_role"
  login = true
  password = "toto"
  valid_until = "2099-05-04 12:00:00+00"
}
`

	var configUpdate = `
resource "postgresql_role" "group_role" {
  name = "group_role"
}

resource "postgresql_role" "update_role" {
  name = "update_role2"
  login = true
  connection_limit = 5
  password = "titi"
  roles = ["${postgresql_role.group_role.name}"]
  search_path = ["mysearchpath"]
  statement_timeout = 30000
  idle_in_transaction_session_timeout = 60000
  assume_role = "${postgresql_role.group_role.name}"
}
`
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckRoleFWDestroy,
		Steps: []resource.TestStep{
			{
				Config: configCreate,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckRoleFWExists("update_role", []string{}, nil),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "name", "update_role"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "login", "true"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "connection_limit", "-1"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "password", "toto"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "valid_until", "2099-05-04 12:00:00+00"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "roles.#", "0"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "search_path.#", "0"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "statement_timeout", "0"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "idle_in_transaction_session_timeout", "0"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "assume_role", ""),
					testAccCheckRoleFWCanLogin(t, "update_role", "toto"),
				),
			},
			{
				Config: configUpdate,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckRoleFWExists("update_role2", []string{"group_role"}, nil),
					resource.TestCheckResourceAttr(
						"postgresql_role.update_role", "name", "update_role2",
					),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "connection_limit", "5"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "login", "true"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "password", "titi"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "valid_until", "infinity"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "roles.#", "1"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "search_path.#", "1"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "search_path.0", "mysearchpath"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "statement_timeout", "30000"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "idle_in_transaction_session_timeout", "60000"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "assume_role", "group_role"),
					testAccCheckRoleFWCanLogin(t, "update_role2", "titi"),
				),
			},
			// apply the first one again to test that the granted role is correctly
			// revoked and the search path has been reset to default.
			{
				Config: configCreate,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckRoleFWExists("update_role", []string{}, nil),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "name", "update_role"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "login", "true"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "connection_limit", "-1"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "password", "toto"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "roles.#", "0"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "search_path.#", "0"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "statement_timeout", "0"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "idle_in_transaction_session_timeout", "0"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "assume_role", ""),
					testAccCheckRoleFWCanLogin(t, "update_role", "toto"),
				),
			},
		},
	})
}

// Test to create a role with admin user (usually postgres) granted to it
// There were a bug on RDS like setup (with a non-superuser postgres role)
// where it couldn't delete the role in this case.
func TestAccRoleFW_AdminGranted(t *testing.T) {
	admin := os.Getenv("PGUSER")
	if admin == "" {
		admin = "postgres"
	}

	roleConfig := fmt.Sprintf(`
resource "postgresql_role" "test_role" {
  name  = "test_role"
  roles = [
	  "%s"
  ]
}`, admin)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			client := testAccProvider.Meta().(*Client)
			db, err := client.Connect()
			if err != nil {
				t.Fatalf("could connect to database: %v", err)
			}
			// Requires >= 9 and <16
			// We disable this test for >= pg16 as it makes no sense with the new createRoleSelfGrant feature
			if !db.featureSupported(featurePrivileges) || db.featureSupported(featureCreateRoleSelfGrant) {
				t.Skipf("Skip extension tests for Postgres %s", db.version)
			}
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckRoleFWDestroy,
		Steps: []resource.TestStep{
			{
				Config: roleConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckRoleFWExists("test_role", []string{admin}, nil),
					resource.TestCheckResourceAttr("postgresql_role.test_role", "name", "test_role"),
				),
			},
		},
	})
}

func TestAccRoleFW_WriteOnlyPassword_Basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccSkipIfTerraformVersionBelow111(t)
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckRoleFWDestroy,
		Steps: []resource.TestStep{
			{
				Config: `
resource "postgresql_role" "wo_pwd_role" {
  name        = "wo_pwd_role"
  login       = true
  password_wo = "secretpass"
  password_wo_version = "1"
}
`,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckRoleFWExists("wo_pwd_role", nil, nil),
					resource.TestCheckNoResourceAttr("postgresql_role.wo_pwd_role", "password_wo"), // must be blank
					resource.TestCheckNoResourceAttr("postgresql_role.wo_pwd_role", "password"),    // must be blank
					testAccCheckRoleFWCanLogin(t, "wo_pwd_role", "secretpass"),                     // actually usable
				),
			},
		},
	})
}

func TestAccRoleFW_WriteOnlyPassword_Switch(t *testing.T) {
	configPassword := `
resource "postgresql_role" "switch_role" {
  name     = "switch_role"
  login    = true
  password = "initialpass"
}`

	configPasswordWO := `
resource "postgresql_role" "switch_role" {
  name        = "switch_role"
  login       = true
  password_wo = "wopass"
  password_wo_version = "1"
}`

	configPasswordWOChangedWithoutVersionChange := `
resource "postgresql_role" "switch_role" {
  name        = "switch_role"
  login       = true
  password_wo = "wopass123"
  password_wo_version = "1"
}`

	configPasswordReturn := `
resource "postgresql_role" "switch_role" {
  name     = "switch_role"
  login    = true
  password = "revertpass"
}`

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccSkipIfTerraformVersionBelow111(t)
			testAccPreCheck(t)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckRoleFWDestroy,
		Steps: []resource.TestStep{
			{
				Config: configPassword,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_role.switch_role", "password", "initialpass"),
					testAccCheckRoleFWCanLogin(t, "switch_role", "initialpass"),
				),
			},
			{
				Config: configPasswordWO,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckRoleFWCanLogin(t, "switch_role", "wopass"),
					resource.TestCheckNoResourceAttr("postgresql_role.switch_role", "password"), // value cleared from state
				),
			},
			{
				Config: configPasswordWOChangedWithoutVersionChange,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckRoleFWCanLogin(t, "switch_role", "wopass"), //because the version didn't change, the old password still works
				),
			},
			{
				Config: configPasswordReturn,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_role.switch_role", "password", "revertpass"),
					testAccCheckRoleFWCanLogin(t, "switch_role", "revertpass"),
				),
			},
		},
	})
}

func TestAccRoleFW_WriteOnlyPassword_WithVersioning(t *testing.T) {
	const roleName = "wo_version_role"

	configWOv1 := `
resource "postgresql_role" "wo_version_role" {
  name                 = "wo_version_role"
  login                = true
  password_wo          = "wopass1"
  password_wo_version  = 1
}`

	configWOv1NewPass := `
resource "postgresql_role" "wo_version_role" {
  name                 = "wo_version_role"
  login                = true
  password_wo          = "wopass2"
  password_wo_version  = "1"
}`

	configWOv2 := `
resource "postgresql_role" "wo_version_role" {
  name                 = "wo_version_role"
  login                = true
  password_wo          = "wopass2"
  password_wo_version  = "2"
}`

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccSkipIfTerraformVersionBelow111(t)
			testAccPreCheck(t)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckRoleFWDestroy,
		Steps: []resource.TestStep{
			{
				Config: configWOv1,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_role.wo_version_role", "password_wo_version", "1"),
					testAccCheckRoleFWCanLogin(t, roleName, "wopass1"),
				),
			},
			{
				// Update the password but keep the version the same → should still login with old password
				Config: configWOv1NewPass,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckRoleFWCanLogin(t, roleName, "wopass1"),
					resource.TestCheckResourceAttr("postgresql_role.wo_version_role", "password_wo_version", "1"),
				),
			},
			{
				// Now bump the version → new password should apply
				Config: configWOv2,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckRoleFWCanLogin(t, roleName, "wopass2"),
					resource.TestCheckResourceAttr("postgresql_role.wo_version_role", "password_wo_version", "2"),
				),
			},
		},
	})
}

// testAccSkipIfTerraformVersionBelow111 skips the calling test when the
// Terraform binary used by the acceptance-test harness is older than 1.11, since
// write-only attributes are only supported on Terraform 1.11+. It honors
// TF_ACC_TERRAFORM_PATH (the same env var the SDK test framework uses to locate
// the binary) and falls back to "terraform" on PATH. Detection failures skip
// conservatively.
func testAccSkipIfTerraformVersionBelow111(t *testing.T) {
	bin := os.Getenv("TF_ACC_TERRAFORM_PATH")
	if bin == "" {
		bin = "terraform"
	}

	out, err := exec.Command(bin, "version", "-json").Output()
	if err != nil {
		t.Skipf("skipping write-only password test: could not determine Terraform version (%s): %v", bin, err)
	}

	var parsed struct {
		TerraformVersion string `json:"terraform_version"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil || parsed.TerraformVersion == "" {
		t.Skipf("skipping write-only password test: could not parse Terraform version output: %v", err)
	}

	if !terraformVersionAtLeast111(parsed.TerraformVersion) {
		t.Skipf("skipping write-only password test: Terraform %s does not support write-only attributes (requires >= 1.11)", parsed.TerraformVersion)
	}
}

// terraformVersionAtLeast111 reports whether the given Terraform version string
// (e.g. "1.6.6" or "1.11.0-beta1") is >= 1.11.0.
func terraformVersionAtLeast111(version string) bool {
	core := version
	if i := strings.IndexAny(core, "-+"); i >= 0 {
		core = core[:i]
	}
	parts := strings.Split(core, ".")
	if len(parts) < 2 {
		return false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	if major != 1 {
		return major > 1
	}
	return minor >= 11
}

func testAccCheckRoleFWDestroy(s *terraform.State) error {
	client := testAccProvider.Meta().(*Client)

	for _, rs := range s.RootModule().Resources {
		if rs.Type != "postgresql_role" {
			continue
		}

		exists, err := checkRoleFWExists(client, rs.Primary.ID)

		if err != nil {
			return fmt.Errorf("error checking role %s", err)
		}

		if exists {
			return fmt.Errorf("Role still exists after destroy")
		}
	}

	return nil
}

func testAccCheckRoleFWExists(roleName string, grantedRoles []string, searchPath []string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client := testAccProvider.Meta().(*Client)

		exists, err := checkRoleFWExists(client, roleName)
		if err != nil {
			return fmt.Errorf("error checking role %s", err)
		}

		if !exists {
			return fmt.Errorf("Role not found")
		}

		if grantedRoles != nil {
			if err := checkRoleFWGrantedRoles(client, roleName, grantedRoles); err != nil {
				return err
			}
		}

		if searchPath != nil {
			if err := checkRoleFWSearchPath(client, roleName, searchPath); err != nil {
				return err
			}
		}
		return nil
	}
}

func checkRoleFWExists(client *Client, roleName string) (bool, error) {
	db, err := client.Connect()
	if err != nil {
		return false, err
	}
	var _rez int
	err = db.QueryRow("SELECT 1 from pg_roles d WHERE rolname=$1", roleName).Scan(&_rez)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("error reading info about role: %s", err)
	}

	return true, nil
}

func testAccCheckRoleFWCanLogin(t *testing.T, role, password string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		config := getTestConfig(t)
		config.Username = role
		config.Password = password
		db, err := sql.Open("postgres", config.connStr("postgres"))
		if err != nil {
			return fmt.Errorf("could not open SQL connection: %v", err)
		}
		if err := db.Ping(); err != nil {
			return fmt.Errorf("could not connect as role %s: %v", role, err)
		}
		return nil
	}
}

func checkRoleFWGrantedRoles(client *Client, roleName string, expectedRoles []string) error {
	db, err := client.Connect()
	if err != nil {
		return err
	}

	rows, err := db.Query(
		"SELECT pg_get_userbyid(roleid) as rolname from pg_auth_members WHERE pg_get_userbyid(member) = $1 ORDER BY rolname",
		roleName,
	)
	if err != nil {
		return fmt.Errorf("error reading granted roles: %v", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			log.Printf("error closing rows: %v", err)
		}
	}()

	grantedRoles := []string{}
	for rows.Next() {
		var grantedRole string
		if err := rows.Scan(&grantedRole); err != nil {
			return fmt.Errorf("error scanning granted role: %v", err)
		}
		grantedRoles = append(grantedRoles, grantedRole)
	}

	sort.Strings(expectedRoles)
	if !reflect.DeepEqual(grantedRoles, expectedRoles) {
		return fmt.Errorf(
			"Role %s is not a member of the expected list of roles. expected %v - got %v",
			roleName, expectedRoles, grantedRoles,
		)
	}
	return nil
}

func checkRoleFWSearchPath(client *Client, roleName string, expectedSearchPath []string) error {
	db, err := client.Connect()
	if err != nil {
		return err
	}

	var searchPathStr string
	err = db.QueryRow(
		"SELECT (pg_options_to_table(rolconfig)).option_value FROM pg_roles WHERE rolname=$1;",
		roleName,
	).Scan(&searchPathStr)

	// The query returns ErrNoRows if the search path hasn't been altered.
	if err != nil && err == sql.ErrNoRows {
		searchPathStr = "\"$user\", public"
	} else if err != nil {
		return fmt.Errorf("error reading search_path: %v", err)
	}

	searchPath := strings.Split(searchPathStr, ", ")
	for i := range searchPath {
		searchPath[i] = strings.Trim(searchPath[i], `"`)
	}
	sort.Strings(expectedSearchPath)
	if !reflect.DeepEqual(searchPath, expectedSearchPath) {
		return fmt.Errorf(
			"search_path is not equal to expected value. expected %v - got %v",
			expectedSearchPath, searchPath,
		)
	}
	return nil
}

var testAccPostgresqlRoleFWConfig = `
resource "postgresql_role" "myrole2" {
  name = "myrole2"
  login = true
}

resource "postgresql_role" "role_with_pwd" {
  name = "role_with_pwd"
  login = true
  password = "mypass"
}

resource "postgresql_role" "role_with_pwd_encr" {
  name = "role_with_pwd_encr"
  login = true
  password = "mypass"
  encrypted_password = true
}

resource "postgresql_role" "role_simple" {
  name = "role_simple"
}

resource "postgresql_role" "role_with_defaults" {
  name = "role_default"
  superuser = false
  create_database = false
  create_role = false
  inherit = false
  login = false
  replication = false
  bypass_row_level_security = false
  connection_limit = -1
  encrypted_password = true
  password = ""
  skip_drop_role = false
  valid_until = "infinity"
  statement_timeout = 0
  idle_in_transaction_session_timeout = 0
  assume_role = ""
}

resource "postgresql_role" "role_with_create_database" {
  name = "role_with_create_database"
  create_database = true
}

resource "postgresql_role" "sub_role" {
	name = "sub_role"
	roles = [
		"${postgresql_role.myrole2.id}",
		"${postgresql_role.role_simple.id}",
	]
}

resource "postgresql_role" "role_with_search_path" {
  name = "role_with_search_path"
  search_path = ["bar", "foo-with-hyphen"]
}
`
