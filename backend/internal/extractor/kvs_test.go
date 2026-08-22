package extractor

import "testing"

func TestKVSRealURL(t *testing.T) {
	// A synthetic fixture: the permutation is what is under test, and it
	// does not care whose host the path belongs to. Real-world correctness
	// is covered by the live tests, which are not tracked here.
	const license = "$112233445566778"

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "high quality",
			in:   "function/0/https://example.test/get_file/5/0123456789abcdef0123456789abcdefaa11bb22cc/10000000/10000001/clip.mp4/",
			want: "https://example.test/get_file/5/f762e3d67821049fb13405c59aedcb8aaa11bb22cc/10000000/10000001/clip.mp4/",
		},
		{
			name: "alternate quality",
			in:   "function/0/https://example.test/get_file/5/fedcba9876543210fedcba9876543210dd33ee44ff/10000000/10000001/clip_240p.mp4/",
			want: "https://example.test/get_file/5/089d1c2987defb604ecbfa3a65123475dd33ee44ff/10000000/10000001/clip_240p.mp4/",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := kvsRealURL(tc.in, license)
			if err != nil {
				t.Fatalf("kvsRealURL: %v", err)
			}
			if got != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestKVSRealURLPassesThroughPlainURLs(t *testing.T) {
	const plain = "https://example.test/media/video.mp4"
	got, err := kvsRealURL(plain, "$123456789")
	if err != nil {
		t.Fatal(err)
	}
	if got != plain {
		t.Errorf("got %q, want it unchanged", got)
	}
}

func TestKVSLicenseTokenRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "$", "$abcdef", "$12x45"} {
		if _, err := kvsLicenseToken(bad); err == nil {
			t.Errorf("kvsLicenseToken(%q) succeeded, want an error", bad)
		}
	}
}

func TestParseFlashvars(t *testing.T) {
	const page = `<script>
	flashvars = {
		video_id: '14496104',
		video_title: 'Example clip \'quoted\' #9',
		license_code: '$112233445566778',
		video_url: 'function/0/https://x.test/get_file/5/abc/1/2/3.mp4/',
		postfix: '.mp4'
	}
	</script>`

	vars, err := parseFlashvars(page)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := vars["video_title"], "Example clip 'quoted' #9"; got != want {
		t.Errorf("video_title = %q, want %q", got, want)
	}
	if got, want := vars["license_code"], "$112233445566778"; got != want {
		t.Errorf("license_code = %q, want %q", got, want)
	}
	if got, want := vars["postfix"], ".mp4"; got != want {
		t.Errorf("postfix = %q, want %q", got, want)
	}
}

func TestParseFlashvarsMissing(t *testing.T) {
	if _, err := parseFlashvars("<html><body>nothing here</body></html>"); err == nil {
		t.Error("want an error when the page has no player configuration")
	}
}
