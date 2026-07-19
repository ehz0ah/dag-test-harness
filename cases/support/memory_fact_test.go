package support

import (
	"reflect"
	"testing"
)

func TestMemoryFactExpectNormalizesAndDeduplicatesTokens(t *testing.T) {
	got := MemoryFactExpect("Marker-123", "Saffron teal", "mango sticky rice", "teal")
	want := []string{"marker-123", "saffron", "teal", "mango", "sticky", "rice"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MemoryFactExpect = %v, want %v", got, want)
	}
}
