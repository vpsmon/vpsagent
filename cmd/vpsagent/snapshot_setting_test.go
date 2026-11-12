package main

import "testing"

func TestSnapshotSettingDefaultsToFollowingCloud(t *testing.T) {
	for _, tc := range []struct {
		value           string
		enabled, follow bool
	}{{"", false, true}, {"  ", false, true}, {"true", true, false}, {"ON", true, false}, {"1", true, false}, {"false", false, false}, {"0", false, false}, {"maybe", false, false}} {
		if enabled, follow := snapshotSetting(tc.value); enabled != tc.enabled || follow != tc.follow {
			t.Fatalf("%q = %v %v, want %v %v", tc.value, enabled, follow, tc.enabled, tc.follow)
		}
	}
}
