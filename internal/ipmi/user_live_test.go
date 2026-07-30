package ipmi

import (
	"os"
	"testing"
)

func findUser(us []User, id byte) *User {
	for i := range us {
		if us[i].ID == id {
			return &us[i]
		}
	}
	return nil
}
func TestUserProvisionLive(t *testing.T) {
	if os.Getenv("IKVM_USER_LIVE") != "1" {
		t.Skip("set IKVM_USER_LIVE=1")
	}
	c, err := Open(os.Getenv("SOL_HOST"), os.Getenv("SOL_USER"), os.Getenv("SOL_PASS"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	const slot = byte(3)
	// provision
	if err := c.SetUserName(slot, "iktest"); err != nil {
		t.Fatalf("SetUserName: %v", err)
	}
	if err := c.SetUserPassword(slot, "Test1234"); err != nil {
		t.Fatalf("SetUserPassword: %v", err)
	}
	if err := c.SetUserPrivilege(slot, 3); err != nil {
		t.Fatalf("SetUserPrivilege: %v", err)
	} // Operator
	if err := c.EnableUser(slot); err != nil {
		t.Fatalf("EnableUser: %v", err)
	}
	// verify
	us, _ := c.ListUsers()
	u := findUser(us, slot)
	if u == nil {
		t.Fatal("slot 3 missing")
	}
	t.Logf("AFTER PROVISION: id=%d name=%q enabled=%v priv=%s", u.ID, u.Name, u.Enabled, u.PrivName)
	if u.Name != "iktest" || !u.Enabled {
		t.Errorf("provision not applied: %+v", *u)
	}
	// cleanup (reversible): disable + clear name
	if err := c.DisableUser(slot); err != nil {
		t.Errorf("DisableUser: %v", err)
	}
	if err := c.SetUserName(slot, ""); err != nil {
		t.Errorf("clear name: %v", err)
	}
	us2, _ := c.ListUsers()
	u2 := findUser(us2, slot)
	t.Logf("AFTER CLEANUP:   id=%d name=%q enabled=%v", u2.ID, u2.Name, u2.Enabled)
	if u2.Name != "" || u2.Enabled {
		t.Errorf("cleanup incomplete: %+v", *u2)
	}
}
