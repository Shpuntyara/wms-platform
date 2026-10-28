package postgres

import "testing"

func TestDurabilitySettingsFailClosed(t *testing.T) {
	t.Parallel()
	for _, commit := range []string{"on", "remote_apply"} {
		t.Run("durable_"+commit, func(t *testing.T) {
			if err := validateDurabilitySettings("on", commit, "on", "on", "off", "off", "off"); err != nil {
				t.Fatalf("durable configuration refused: %v", err)
			}
		})
	}
	for _, value := range []string{"", "off", "local", "remote_write", "true", "unexpected"} {
		t.Run("weak_commit_"+value, func(t *testing.T) {
			if err := validateDurabilitySettings("on", value, "on", "on", "off", "off", "off"); err == nil {
				t.Fatalf("accepted weak or missing synchronous_commit %q", value)
			}
		})
	}
	for index, name := range []string{"fsync", "full_page_writes", "data_checksums"} {
		for _, value := range []string{"", "off", "true", "unexpected"} {
			t.Run(name+"_"+value, func(t *testing.T) {
				flags := []string{"on", "on", "on"}
				flags[index] = value
				if err := validateDurabilitySettings(flags[0], "on", flags[1], flags[2], "off", "off", "off"); err == nil {
					t.Fatalf("accepted unsafe %s=%q", name, value)
				}
			})
		}
	}
	for index, name := range []string{"ignore_checksum_failure", "zero_damaged_pages", "ignore_invalid_pages"} {
		for _, value := range []string{"", "on", "false", "unexpected"} {
			t.Run(name+"_"+value, func(t *testing.T) {
				flags := []string{"off", "off", "off"}
				flags[index] = value
				if err := validateDurabilitySettings("on", "on", "on", "on", flags[0], flags[1], flags[2]); err == nil {
					t.Fatalf("accepted corruption bypass %s=%q", name, value)
				}
			})
		}
	}
}
