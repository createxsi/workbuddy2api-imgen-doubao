package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// 只写 enum 不写 type 的属性补推断类型；推不出唯一类型、已显式 type 的不动。
func TestToolSchemaEnumTypeInferred(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{` +
		`"d":{"enum":["x","y"]},"n":{"enum":[1,2]},"r":{"enum":[1,2.5]},"b":{"enum":[true,false]},` +
		`"mix":{"enum":["a",1]},"nullish":{"enum":[null,"a"]},"typed":{"type":"string","enum":["p"]},` +
		`"list":{"type":"array","items":{"enum":["a"]}},"one":{"anyOf":[{"enum":["z"]}]}}}}}]}`
	out := PrepareBodyOpt([]byte(in), false)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	props := got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)["properties"].(map[string]any)
	want := map[string]string{"d": "string", "n": "integer", "r": "number", "b": "boolean", "mix": "", "nullish": "", "typed": "string"}
	for k, w := range want {
		p := props[k].(map[string]any)
		tp, has := p["type"].(string)
		if w == "" && has {
			t.Errorf("%s 不该补 type，得到 %q", k, tp)
		}
		if w != "" && tp != w {
			t.Errorf("%s type=%q want %q", k, tp, w)
		}
		if _, ok := p["enum"]; !ok {
			t.Errorf("%s enum 被删", k)
		}
	}
	if tp := props["list"].(map[string]any)["items"].(map[string]any)["type"]; tp != "string" {
		t.Errorf("items 内未补: %v", tp)
	}
	if tp := props["one"].(map[string]any)["anyOf"].([]any)[0].(map[string]any)["type"]; tp != "string" {
		t.Errorf("anyOf 内未补: %v", tp)
	}
}

// response_format 的 enum 是输出约束，不补类型。
func TestToolSchemaEnumTypeLeavesResponseFormat(t *testing.T) {
	in := `{"model":"m","messages":[],"response_format":{"type":"json_schema","json_schema":{"schema":{"properties":{"k":{"enum":["a"]}}}}}}`
	out := string(PrepareBodyOpt([]byte(in), false))
	if !strings.Contains(out, `"k":{"enum":["a"]}`) {
		t.Fatalf("response_format 被改: %s", out)
	}
}
