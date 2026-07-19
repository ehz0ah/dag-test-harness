package support

import (
	"reflect"
	"testing"
)

func TestRemoteResourceDefaultsAreStableAndExtractionTolerant(t *testing.T) {
	t.Setenv("OV_TEST_REMOTE_RESOURCE_URL", "")
	t.Setenv("OV_TEST_REMOTE_RESOURCE_EXPECT", "")
	if got := RemoteResourceURL(""); got != DefaultRemoteResourceURL {
		t.Fatalf("RemoteResourceURL() = %q, want %q", got, DefaultRemoteResourceURL)
	}
	want := []string{"celeste fpga", "basys3", "verilog"}
	if got := RemoteResourceExpect(""); !reflect.DeepEqual(got, want) {
		t.Fatalf("RemoteResourceExpect() = %v, want %v", got, want)
	}
}

func TestRemoteResourceHarnessOverridesTakePrecedence(t *testing.T) {
	t.Setenv("OV_TEST_REMOTE_RESOURCE_URL", "https://common.example/resource")
	t.Setenv("OV_TEST_REMOTE_RESOURCE_EXPECT", "common, markers")
	t.Setenv("OV_TEST_HARNESS_RESOURCE_URL", "https://harness.example/resource")
	t.Setenv("OV_TEST_HARNESS_RESOURCE_EXPECT", " Alpha, BETA , ")
	if got, want := RemoteResourceURL("OV_TEST_HARNESS_RESOURCE_URL"), "https://harness.example/resource"; got != want {
		t.Fatalf("RemoteResourceURL() = %q, want %q", got, want)
	}
	want := []string{"alpha", "beta"}
	if got := RemoteResourceExpect("OV_TEST_HARNESS_RESOURCE_EXPECT"); !reflect.DeepEqual(got, want) {
		t.Fatalf("RemoteResourceExpect() = %v, want %v", got, want)
	}
}
