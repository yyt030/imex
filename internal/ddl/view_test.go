package ddl

import (
	"strings"
	"testing"
)

// These cases use the exact GET_DDL('VIEW', ...) text measured on the live
// dm8_single instance in this sandbox (see docs/requirements.md, this round).
// The parser must split the CREATE shell from the SELECT body, and the renderer
// must print MySQL grammar without touching the SELECT's meaning.
func TestParseDamengView(t *testing.T) {
	cases := []struct {
		name        string
		ddl         string
		wantName    string
		wantSchema  string
		wantBody    string
		wantCheck   string
		wantReplace bool
	}{
		{
			name:     "bare view",
			ddl:      `CREATE OR REPLACE VIEW FX_V AS SELECT ID, A FROM FX_SRC;`,
			wantName: "FX_V", wantBody: "SELECT ID, A FROM FX_SRC", wantReplace: true,
		},
		{
			name:     "qualified view with check option",
			ddl:      `CREATE  OR REPLACE  VIEW DMPROBE.FX_V_CO AS  SELECT FX_SRC.ID,FX_SRC.A FROM DMPROBE.FX_SRC WHERE ID > 0 WITH CASCADED CHECK OPTION;`,
			wantName: "FX_V_CO", wantSchema: "DMPROBE",
			wantBody:  "SELECT FX_SRC.ID,FX_SRC.A FROM DMPROBE.FX_SRC WHERE ID > 0",
			wantCheck: "CASCADED", wantReplace: true,
		},
		{
			name:     "quoted identifiers",
			ddl:      `CREATE VIEW "DMPROBE"."FX_V" AS SELECT "ID","A" FROM "FX_SRC";`,
			wantName: "FX_V", wantSchema: "DMPROBE", wantBody: `SELECT "ID","A" FROM "FX_SRC"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := parseDamengView(tc.ddl)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if v.Name != tc.wantName || v.Schema != tc.wantSchema {
				t.Errorf("name/schema = %q/%q, want %q/%q", v.Name, v.Schema, tc.wantName, tc.wantSchema)
			}
			if v.Body != tc.wantBody {
				t.Errorf("body = %q, want %q", v.Body, tc.wantBody)
			}
			if v.CheckOption != tc.wantCheck {
				t.Errorf("check = %q, want %q", v.CheckOption, tc.wantCheck)
			}
			if v.OrReplace != tc.wantReplace {
				t.Errorf("orReplace = %v, want %v", v.OrReplace, tc.wantReplace)
			}
		})
	}
}

func TestRenderMySQLView(t *testing.T) {
	cases := []struct {
		name string
		ddl  string
		want string
	}{
		{
			name: "bare view",
			ddl:  `CREATE OR REPLACE VIEW FX_V AS SELECT ID, A FROM FX_SRC;`,
			want: "CREATE OR REPLACE VIEW `FX_V` AS SELECT ID, A FROM FX_SRC",
		},
		{
			name: "qualified with check option",
			ddl:  `CREATE OR REPLACE VIEW DMPROBE.FX_V_CO AS SELECT FX_SRC.ID FROM DMPROBE.FX_SRC WHERE ID > 0 WITH CASCADED CHECK OPTION;`,
			want: "CREATE OR REPLACE VIEW `DMPROBE`.`FX_V_CO` AS SELECT FX_SRC.ID FROM DMPROBE.FX_SRC WHERE ID > 0 WITH CASCADED CHECK OPTION",
		},
		{
			name: "quoted identifiers become backquotes",
			ddl:  `CREATE VIEW "DMPROBE"."FX_V" AS SELECT "ID","A" FROM "FX_SRC";`,
			want: "CREATE VIEW `DMPROBE`.`FX_V` AS SELECT `ID`,`A` FROM `FX_SRC`",
		},
		{
			name: "NVL maps to COALESCE",
			ddl:  `CREATE VIEW FX_V AS SELECT NVL(A, B) AS X FROM FX_SRC;`,
			want: "CREATE VIEW `FX_V` AS SELECT COALESCE(A, B) AS X FROM FX_SRC",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := parseDamengView(tc.ddl)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got, err := RenderMySQLView(v)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if got != tc.want {
				t.Errorf("render:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// An Oracle construct with no faithful MySQL spelling is a refusal, never a
// printed statement that might parse with a different meaning. The most
// dangerous is || (concatenation in Oracle, logical OR in MySQL).
func TestRenderMySQLViewRefusals(t *testing.T) {
	cases := []struct {
		name string
		ddl  string
		want string
	}{
		{"concat operator", `CREATE VIEW V AS SELECT A || B FROM T;`, "||"},
		{"rownum", `CREATE VIEW V AS SELECT ROWNUM, A FROM T;`, "ROWNUM"},
		{"decode", `CREATE VIEW V AS SELECT DECODE(A,1,'x','y') FROM T;`, "DECODE"},
		{"oracle outer join", `CREATE VIEW V AS SELECT A FROM T1,T2 WHERE T1.ID=T2.ID(+);`, "(+)"},
		{"connect by", `CREATE VIEW V AS SELECT ID FROM T CONNECT BY PRIOR ID=PID;`, "CONNECT"},
		{"nvl2", `CREATE VIEW V AS SELECT NVL2(A,B,C) FROM T;`, "NVL2"},
		{"sysdate", `CREATE VIEW V AS SELECT SYSDATE FROM T;`, "SYSDATE"},
		{"minus", `CREATE VIEW V AS SELECT A FROM T1 MINUS SELECT A FROM T2;`, "MINUS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := parseDamengView(tc.ddl)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			_, err = RenderMySQLView(v)
			if err == nil {
				t.Fatalf("render of %q unexpectedly succeeded", tc.ddl)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %q does not name %q", err, tc.want)
			}
		})
	}
}

// A quoted string that merely contains an Oracle word is data, not a construct:
// copying it through must not trip the refusal scanner.
func TestTranslateViewBodySkipsStringLiterals(t *testing.T) {
	in := `SELECT 'ROWNUM || DECODE' AS LIT, A FROM T`
	want := in
	got, err := translateViewBody(in)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestConvertibleKinds(t *testing.T) {
	for _, kind := range []string{"table", "view"} {
		if _, ok := ConvertibleKind(kind); !ok {
			t.Errorf("%s should be convertible", kind)
		}
	}
	for _, kind := range []string{"trigger", "procedure", "function", "schema", "event", "weird"} {
		reason, ok := ConvertibleKind(kind)
		if ok {
			t.Errorf("%s should not be convertible", kind)
		}
		if reason == "" {
			t.Errorf("%s refused without a reason", kind)
		}
	}
}
