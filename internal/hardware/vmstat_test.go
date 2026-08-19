package hardware

import "testing"

// The fixtures below are verbatim `vm_stat` output captured from the two
// Macs in the fleet on 2026-08-19 (waired-ai/waired-agent#835). They are
// captured rather than hand-authored because the arithmetic depends on
// page classes the previous hand-authored fixture did not have:
// "File-backed pages" and "Anonymous pages" only appear on a real host,
// and it was their absence that let the omission ship.
//
// macminiCacheSmall / macminiCacheLarge are the same host before and
// after reading one 20 GB file (`dd ... of=/dev/null`), which is the
// closest reachable stand-in for the moment the install-time
// measurement is taken — right after a multi-GB model download.

// sv-macmini, Apple M4, 16 GiB, macOS 26.5.1, with the file cache in its
// everyday state: 409,145 file-backed pages against an inactive list of
// 388,196, so only 19,677 of them can be proven to sit outside the sum.
const macminiCacheSmall = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                    24141.
Pages active:                                 383318.
Pages inactive:                               388196.
Pages speculative:                              1272.
Pages throttled:                                   0.
Pages wired down:                             106741.
Pages purgeable:                                4133.
"Translation faults":                     1896659814.
Pages copy-on-write:                       157695573.
Pages zero filled:                        1177536752.
Pages reactivated:                          16332485.
Pages purged:                               35288602.
File-backed pages:                            409145.
Anonymous pages:                              363641.
Pages stored in compressor:                   478454.
Pages occupied by compressor:                 110063.
Decompressions:                             15197681.
Compressions:                               17826870.
Pageins:                                    42255403.
Pageouts:                                     775402.
Swapins:                                      187488.
Swapouts:                                     332213.
`

// The same host seconds later, after reading one 20 GB file. Free pages
// collapsed 24,141 → 3,812 and the file cache grew 409,145 → 470,266
// pages: the old sum FELL by ~0.47 GB while ~1 GB of instantly
// reclaimable cache was added.
const macminiCacheLarge = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                     3812.
Pages active:                                 385486.
Pages inactive:                               383693.
Pages speculative:                              1357.
Pages throttled:                                   0.
Pages wired down:                             106683.
Pages purgeable:                                  14.
"Translation faults":                     1896676113.
Pages copy-on-write:                       157696665.
Pages zero filled:                        1177542102.
Pages reactivated:                          16361694.
Pages purged:                               35291466.
File-backed pages:                            470266.
Anonymous pages:                              300270.
Pages stored in compressor:                   527626.
Pages occupied by compressor:                 132936.
Decompressions:                             15198825.
Compressions:                               17878645.
Pageins:                                    43535829.
Pageouts:                                     775420.
Swapins:                                      187488.
Swapouts:                                     332213.
`

// pc-mbp14-m5, Apple M5 Pro, 48 GiB, macOS 26.6.2 — the host in #835.
// Carries the "Pages tagged*" lines macOS 26.6 added, which nothing here
// reads and which must stay harmless.
const mbp14M5Pro = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                  1133377.
Pages active:                                 683548.
Pages inactive:                               770303.
Pages speculative:                            210943.
Pages throttled:                                   0.
Pages wired down:                             160600.
Pages purgeable:                               23665.
"Translation faults":                      129543927.
Pages copy-on-write:                         8702476.
Pages zero filled:                         225374213.
Pages reactivated:                            626262.
Pages purged:                                 662021.
File-backed pages:                            896753.
Anonymous pages:                              768041.
Pages stored in compressor:                   268292.
Pages occupied by compressor:                 120819.
Decompressions:                               463940.
Compressions:                                1068212.
Pageins:                                     7190431.
Pageouts:                                     312695.
Swapins:                                           0.
Swapouts:                                          0.
Pages tagged:                                 141552.
Pages tagged resident:                        113239.
Pages tagged compressed:                       28313.
Pages tag-storage:                             98304.
Pages tag-storage holding tags:                 4841.
Pages tag-storage free:                         5724.
Pages tag-storage non-tag pageable:            87731.
Pages tag-storage non-tag wired:                   8.
`

// The pre-#835 fixture, kept verbatim: hand-authored, and with no
// "File-backed pages" line. It is the shape of an older macOS, and it
// pins that such a host's figure did not move at all.
const vmStatNoFileBackedLine = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                    14921.
Pages active:                                 429524.
Pages inactive:                               423000.
Pages speculative:                             25227.
Pages throttled:                                   0.
Pages wired down:                              84981.
Pages purgeable:                                2141.
"Translation faults":                      123456789.
`

func TestParseVMStatAvailableBytes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// wantPages is the page count, spelled as the sum it is so a
		// reader can check the arithmetic against the fixture above.
		wantPages uint64
	}{
		{
			name: "an everyday file cache adds only its provable remainder",
			in:   macminiCacheSmall,
			// free + inactive + speculative + purgeable, plus
			// max(0, 409145 - (388196+1272)) = 19677 active file pages.
			wantPages: 24141 + 388196 + 1272 + 4133 + 19677,
		},
		{
			name: "a large file cache is counted, not charged to the OS",
			in:   macminiCacheLarge,
			// max(0, 470266 - (383693+1357)) = 85216 active file pages.
			wantPages: 3812 + 383693 + 1357 + 14 + 85216,
		},
		{
			name: "cache under the inactive list adds nothing, unknown lines ignored",
			in:   mbp14M5Pro,
			// 896753 < 770303+210943, so nothing can be proven active.
			wantPages: 1133377 + 770303 + 210943 + 23665,
		},
		{
			name: "no File-backed line: byte-for-byte the pre-#835 figure",
			in:   vmStatNoFileBackedLine,
			// The sum this function has always returned.
			wantPages: 14921 + 423000 + 25227 + 2141,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseVMStatAvailableBytes([]byte(c.in))
			if err != nil {
				t.Fatalf("parseVMStatAvailableBytes returned error: %v", err)
			}
			if want := c.wantPages * 16384; got != want {
				t.Errorf("parseVMStatAvailableBytes = %d bytes (%d pages), want %d (%d pages)",
					got, got/16384, want, c.wantPages)
			}
		})
	}

	t.Run("the large cache reads as more available than the small one", func(t *testing.T) {
		// The regression #835 is about, stated as the comparison a user
		// makes: the same host, seconds apart, with MORE reclaimable
		// memory must not report less of it. Before this change the
		// inequality ran the other way.
		small, err := parseVMStatAvailableBytes([]byte(macminiCacheSmall))
		if err != nil {
			t.Fatalf("small: %v", err)
		}
		large, err := parseVMStatAvailableBytes([]byte(macminiCacheLarge))
		if err != nil {
			t.Fatalf("large: %v", err)
		}
		if large <= small {
			t.Errorf("available after filling the file cache = %d, want > %d", large, small)
		}
	})

	t.Run("missing page-size header errors", func(t *testing.T) {
		if _, err := parseVMStatAvailableBytes([]byte("Pages free: 100.\n")); err == nil {
			t.Error("expected error for missing page-size header, got nil")
		}
	})
	t.Run("missing Pages free errors", func(t *testing.T) {
		in := "Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages active: 100.\n"
		if _, err := parseVMStatAvailableBytes([]byte(in)); err == nil {
			t.Error("expected error for missing 'Pages free' line, got nil")
		}
	})
	t.Run("absent optional classes count as zero", func(t *testing.T) {
		// Only free present; inactive/speculative/purgeable absent.
		in := "Mach Virtual Memory Statistics: (page size of 4096 bytes)\nPages free: 10.\n"
		got, err := parseVMStatAvailableBytes([]byte(in))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != uint64(10)*4096 {
			t.Errorf("got %d, want %d", got, uint64(10)*4096)
		}
	})
}

// TestVMStatFixturesPartitionTheResidentSet checks the identity the
// active-file-backed bound is derived from — File-backed + Anonymous =
// active + inactive + speculative — on every captured fixture. If a
// future macOS breaks it, the bound in activeFileBackedPages stops being
// a bound, and this is the test that says so rather than a wrong
// capacity verdict on somebody's Mac.
func TestVMStatFixturesPartitionTheResidentSet(t *testing.T) {
	for _, c := range []struct {
		name                          string
		fileBacked, anonymous         uint64
		active, inactive, speculative uint64
	}{
		{"macmini cache small", 409145, 363641, 383318, 388196, 1272},
		{"macmini cache large", 470266, 300270, 385486, 383693, 1357},
		{"mbp14 m5 pro", 896753, 768041, 683548, 770303, 210943},
	} {
		t.Run(c.name, func(t *testing.T) {
			resident := c.active + c.inactive + c.speculative
			if got := c.fileBacked + c.anonymous; got != resident {
				t.Errorf("File-backed + Anonymous = %d, want %d (active+inactive+speculative)",
					got, resident)
			}
		})
	}
}

func TestActiveFileBackedPages(t *testing.T) {
	cases := []struct {
		name   string
		counts map[string]uint64
		want   uint64
	}{
		{
			name:   "no File-backed line",
			counts: map[string]uint64{"Pages inactive": 100, "Pages speculative": 10},
			want:   0,
		},
		{
			name: "cache entirely accounted for by inactive and speculative",
			counts: map[string]uint64{
				"File-backed pages": 100, "Pages inactive": 90, "Pages speculative": 10,
			},
			want: 0,
		},
		{
			name: "the remainder is the bound",
			counts: map[string]uint64{
				"File-backed pages": 250, "Pages inactive": 90, "Pages speculative": 10,
			},
			want: 150,
		},
		{
			name:   "cache with nothing inactive is all of it",
			counts: map[string]uint64{"File-backed pages": 250},
			want:   250,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := activeFileBackedPages(c.counts); got != c.want {
				t.Errorf("activeFileBackedPages = %d, want %d", got, c.want)
			}
		})
	}
}
