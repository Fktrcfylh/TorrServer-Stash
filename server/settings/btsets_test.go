package settings

import "testing"

func TestSetDefaultConfigDefaultPinNext(t *testing.T) {
	withTestDB(t)
	SetDefaultConfig()
	if BTsets.DefaultPinNext != 3 {
		t.Fatalf("DefaultPinNext = %d, want 3", BTsets.DefaultPinNext)
	}
}

func TestSetBTSetsDefaultPinNext(t *testing.T) {
	withTestDB(t)
	for _, tc := range []struct{ in, want int }{{0, 3}, {-1, 3}, {5, 5}} {
		SetBTSets(&BTSets{DefaultPinNext: tc.in})
		if BTsets.DefaultPinNext != tc.want {
			t.Errorf("SetBTSets(%d): DefaultPinNext = %d, want %d", tc.in, BTsets.DefaultPinNext, tc.want)
		}
	}
}

func TestLoadBTSetsDefaultPinNext(t *testing.T) {
	withTestDB(t)
	for _, tc := range []struct {
		stored string
		want   int
	}{
		{`{"CacheSize":1}`, 3},
		{`{"CacheSize":1,"DefaultPinNext":0}`, 3},
		{`{"CacheSize":1,"DefaultPinNext":7}`, 7},
	} {
		tdb.Set("Settings", "BitTorr", []byte(tc.stored))
		BTsets = nil
		loadBTSets()
		if BTsets.DefaultPinNext != tc.want {
			t.Errorf("loadBTSets(%s): DefaultPinNext = %d, want %d", tc.stored, BTsets.DefaultPinNext, tc.want)
		}
	}
}
