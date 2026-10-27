package skills

import "testing"

// Found live (2026-10-21): find given the shared folder's link printed the link and nothing in it, and the
// engine took the person's folder for empty.
func TestAFindFromTheSharedLinkLooksInsideIt(t *testing.T) {
	cases := map[string]string{
		`find /home/headless/PC -maxdepth 4 -iname '*Kavya*'`:                   `find /home/headless/PC/ -maxdepth 4 -iname '*Kavya*'`,
		`find /home/headless/work /home/headless/PC -maxdepth 3 -name x | sort`: `find /home/headless/work /home/headless/PC/ -maxdepth 3 -name x | sort`,
		`ls ~/PC && find ~/PC -type f`:                                          `ls ~/PC && find ~/PC/ -type f`,
		`find "$HOME/PC" -name '*.xlsx'`:                                        `find "$HOME/PC/" -name '*.xlsx'`,
		`find /home/headless/PC`:                                                `find /home/headless/PC/`,
		`cd /tmp; find /home/headless/PC \( -iname a -o -iname b \)`:            `cd /tmp; find /home/headless/PC/ \( -iname a -o -iname b \)`,
	}
	for in, want := range cases {
		if got := intoSharedLink(in); got != want {
			t.Errorf("intoSharedLink(%q)\n got %q\nwant %q", in, got, want)
		}
	}
	// Left as written: a find that follows links, a path inside the folder, another folder, no find at all.
	for _, same := range []string{
		`find -L /home/headless/PC -name x`,
		`find /home/headless/PC/ -name x`,
		`find "/home/headless/PC/GST Returns" -name x`,
		`find /home/headless/work -name PC`,
		`ls -R /home/headless/PC`,
		`office read /home/headless/PC/x.xlsx`,
	} {
		if got := intoSharedLink(same); got != same {
			t.Errorf("intoSharedLink(%q) changed it to %q", same, got)
		}
	}
}
