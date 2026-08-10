package pool

import "testing"

func TestParseLine(t *testing.T) {
	cases := []struct {
		in   string
		typ  string
		host string
		port int
		user string
		pass string
		ok   bool
	}{
		{"http://u:p@1.2.3.4:8080", "http", "1.2.3.4", 8080, "u", "p", true},
		{"socks5://u:p@1.2.3.4:1080", "socks5", "1.2.3.4", 1080, "u", "p", true},
		{"http://1.2.3.4:8080", "http", "1.2.3.4", 8080, "", "", true},
		{"1.2.3.4:3128", "http", "1.2.3.4", 3128, "", "", true},
		{"https://1.2.3.4:3129", "http", "1.2.3.4", 3129, "", "", true}, // https normalized to http
		{"ftp://x:3128", "", "", 0, "", "", false},
		{"http://no-port", "", "", 0, "", "", false},
		{"", "", "", 0, "", "", false},
	}
	for _, c := range cases {
		p, err := ParseLine(c.in)
		if c.ok {
			if err != nil {
				t.Errorf("%q: unexpected error %v", c.in, err)
				continue
			}
			if p.Type != c.typ || p.Host != c.host || p.Port != c.port || p.Username != c.user || p.Password != c.pass {
				t.Errorf("%q: got %+v", c.in, p)
			}
		} else if err == nil {
			t.Errorf("%q: expected error", c.in)
		}
	}
}

func TestImportDedupeAndPrune(t *testing.T) {
	m := NewManager()
	parsed, invalid := ParseBatch("http://a:b@1.1.1.1:80\nsocks5://c:d@2.2.2.2:1080\nbad line\nhttp://a:b@1.1.1.1:80")
	if len(invalid) != 1 {
		t.Fatalf("invalid = %v", invalid)
	}
	added, skipped, ids := m.Import(parsed)
	if added != 2 || skipped != 1 {
		t.Fatalf("added=%d skipped=%d", added, skipped)
	}
	if len(m.All()) != 2 {
		t.Fatalf("pool size = %d", len(m.All()))
	}
	// freshly imported proxies are "unproven"; mark both usable first
	for _, id := range ids {
		m.SetUsable(id, true)
	}
	// mark one dead
	m.SetUsable(ids[0], false)
	removed, _ := m.Prune()
	if removed != 1 {
		t.Fatalf("prune removed %d, want 1", removed)
	}
	if len(m.All()) != 1 {
		t.Fatalf("pool size after prune = %d", len(m.All()))
	}
}

func TestManualProxy(t *testing.T) {
	p, err := ParseLine("http://xtyig4c0qcoq:o6rnuehdluyhxoz@65.111.21.226:3129")
	if err != nil {
		t.Fatal(err)
	}
	if p.Host != "65.111.21.226" || p.Port != 3129 || p.Username != "xtyig4c0qcoq" || p.Password != "o6rnuehdluyhxoz" {
		t.Fatalf("parsed wrong: %+v", p)
	}
}
