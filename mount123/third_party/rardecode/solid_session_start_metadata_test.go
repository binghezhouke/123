package rardecode

import "testing"

func TestSolidSessionStartEligibilityFromParsedHeaders(t *testing.T) {
	for _, tc := range []struct {
		name string
		want []bool
	}{
		{"rar3-two-groups.rar", []bool{true, false, true, false}},
		{"rar5-two-groups.rar", []bool{true, false, true, false}},
		{"rar3-two-groups-multipart.part01.rar", []bool{true, false, true, false}},
		{"rar5-two-groups-multipart.part01.rar", []bool{true, false, true, false}},
		{"rar3-vols.part1.rar", []bool{false, false}},
		{"rar5-vols.part1.rar", []bool{false, false}},
		{"rar5-group-boundaries.rar", []bool{false, true, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ordinal := 0
			err := Walk(tc.name, func(h *FileHeader) error {
				if ordinal >= len(tc.want) {
					t.Fatal("unexpected extra header")
				}
				if h.CanStartSolidSession() != tc.want[ordinal] {
					t.Fatalf("member %d eligibility=%t want=%t", ordinal, h.CanStartSolidSession(), tc.want[ordinal])
				}
				ordinal++
				return nil
			}, FileSystem(solidFixtureFS()))
			if err != nil || ordinal != len(tc.want) {
				t.Fatalf("walk %d headers, error %v", ordinal, err)
			}
		})
	}
}
