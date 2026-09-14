package utils

import "testing"

func TestIsVideoFile(t *testing.T) {
	cases := map[string]bool{
		"Show.S01E01.mkv": true,
		"dir/Movie.MP4":   true,
		"Show.S01E01.srt": false,
		"Show.S01E01.mka": false,
		"README":          false,
	}
	for path, want := range cases {
		if got := IsVideoFile(path); got != want {
			t.Errorf("IsVideoFile(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestIsSampleFile(t *testing.T) {
	cases := map[string]bool{
		"Show.S01E01.sample.mkv": true,
		"trailer.mp4":            true,
		"dir/Sample/x.mkv":       false, // only the base name is matched
		"Show.S01E01.mkv":        false,
		"Samples.mkv":            false,
	}
	for path, want := range cases {
		if got := IsSampleFile(path); got != want {
			t.Errorf("IsSampleFile(%q) = %v, want %v", path, got, want)
		}
	}
}
