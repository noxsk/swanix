package config

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
)

type uciSection struct {
	typ     string
	name    string
	line    int
	options []uciOption
}

type uciOption struct {
	key    string
	value  string
	isList bool
	line   int
}

// LoadFile reads and decodes a UCI file such as /etc/config/swanix. The result
// is decoded strictly but not validated; call Validate for semantic checks.
func LoadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(string(data))
}

// Parse decodes UCI text.
func Parse(text string) (*Config, error) {
	sections, err := scanUCI(text)
	if err != nil {
		return nil, err
	}
	return decode(sections)
}

func splitWords(line string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == ' ' || r == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		case r == '#' && !inWord:
			if inWord {
				words = append(words, cur.String())
			}
			return words, nil
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote")
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

func scanUCI(text string) ([]*uciSection, error) {
	var sections []*uciSection
	var cur *uciSection
	for i, raw := range strings.Split(text, "\n") {
		ln := i + 1
		words, err := splitWords(strings.TrimRight(raw, "\r"))
		if err != nil {
			return nil, Error{Path: fmt.Sprintf("line %d", ln), Reason: err.Error()}
		}
		if len(words) == 0 {
			continue
		}
		switch words[0] {
		case "package":
		case "config":
			if len(words) < 2 || len(words) > 3 {
				return nil, Error{Path: fmt.Sprintf("line %d", ln), Reason: "config 需要类型和可选名称"}
			}
			cur = &uciSection{typ: words[1], line: ln}
			if len(words) == 3 {
				cur.name = words[2]
			}
			sections = append(sections, cur)
		case "option", "list":
			if cur == nil {
				return nil, Error{Path: fmt.Sprintf("line %d", ln), Reason: words[0] + " 出现在任何 config 之前"}
			}
			if len(words) != 3 {
				return nil, Error{Path: fmt.Sprintf("line %d", ln), Reason: words[0] + " 需要键和值"}
			}
			cur.options = append(cur.options, uciOption{key: words[1], value: words[2], isList: words[0] == "list", line: ln})
		default:
			return nil, Error{Path: fmt.Sprintf("line %d", ln), Reason: "未知语句 " + words[0]}
		}
	}
	return sections, nil
}

func decode(sections []*uciSection) (*Config, error) {
	cfg := Defaults()
	var errs Errors
	seenNames := map[string]string{}
	singletons := map[string]bool{}
	for _, s := range sections {
		path := s.typ
		if s.name != "" {
			path = s.typ + "." + s.name
		}
		if s.name != "" {
			if prev, dup := seenNames[s.name]; dup {
				errs.addf(path, "每个具名 section 在整个文件中必须唯一", "名称 %q 已被 %s 使用", s.name, prev)
				continue
			}
			seenNames[s.name] = s.typ
		}
		switch s.typ {
		case "global", "kernel", "ipv6", "proxy":
			if singletons[s.typ] {
				errs.addf(path, "只保留一个该类型 section", "重复的 %s section", s.typ)
				continue
			}
			singletons[s.typ] = true
			var target any
			switch s.typ {
			case "global":
				target = &cfg.Global
			case "kernel":
				target = &cfg.Kernel
			case "ipv6":
				target = &cfg.IPv6
			case "proxy":
				target = &cfg.Proxy
			}
			decodeSection(s, path, target, &errs)
		case "wan", "policy", "rule", "health":
			if s.name == "" {
				errs.addf(path, "为该 section 指定名称，例如 config "+s.typ+" 'name'", "第 %d 行：%s 必须具名", s.line, s.typ)
				continue
			}
			switch s.typ {
			case "wan":
				w := WAN{Name: s.name, Enabled: true, Preference: 100, Role: RoleActive, TrafficBias: BiasBalanced, IPv6Network: "auto"}
				decodeSection(s, path, &w, &errs)
				cfg.WANs = append(cfg.WANs, w)
			case "policy":
				p := Policy{Name: s.name, Affinity: AffinityConnection, AffinityTimeout: 300, UnavailableAction: UnavailableReject}
				decodeSection(s, path, &p, &errs)
				cfg.Policies = append(cfg.Policies, p)
			case "rule":
				r := Rule{Name: s.name, Enabled: true, Priority: 100}
				decodeSection(s, path, &r, &errs)
				cfg.Rules = append(cfg.Rules, r)
			case "health":
				h := Health{Name: s.name}
				decodeSection(s, path, &h, &errs)
				cfg.Health = append(cfg.Health, h.withDefaults())
			}
		default:
			errs.addf(path, "删除该 section 或修正类型", "未知 section 类型 %q", s.typ)
		}
	}
	if err := errs.OrNil(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func decodeSection(s *uciSection, path string, target any, errs *Errors) {
	v := reflect.ValueOf(target).Elem()
	t := v.Type()
	fields := map[string]int{}
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("uci")
		if tag != "" && tag != "-" {
			fields[tag] = i
		}
	}
	listReset := map[string]bool{}
	for _, o := range s.options {
		idx, ok := fields[o.key]
		opath := path + "." + o.key
		if !ok {
			errs.addf(opath, "删除该字段", "未知字段（第 %d 行）", o.line)
			continue
		}
		f := v.Field(idx)
		switch f.Kind() {
		case reflect.Slice:
			if !listReset[o.key] {
				f.Set(reflect.Zero(f.Type()))
				listReset[o.key] = true
			}
			f.Set(reflect.Append(f, reflect.ValueOf(o.value)))
		default:
			if o.isList {
				errs.addf(opath, "改用 option", "该字段不是列表（第 %d 行）", o.line)
				continue
			}
			if err := setScalar(f, o.value); err != nil {
				errs.addf(opath, err.Error(), "第 %d 行的值 %q 无效", o.line, o.value)
			}
		}
	}
}

func setScalar(f reflect.Value, val string) error {
	switch f.Kind() {
	case reflect.String:
		f.SetString(val)
	case reflect.Int:
		n, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("需要整数")
		}
		f.SetInt(int64(n))
	case reflect.Float64:
		n, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return fmt.Errorf("需要数字")
		}
		f.SetFloat(n)
	case reflect.Bool:
		switch strings.ToLower(val) {
		case "1", "true", "yes", "on", "enabled":
			f.SetBool(true)
		case "0", "false", "no", "off", "disabled":
			f.SetBool(false)
		default:
			return fmt.Errorf("需要 0 或 1")
		}
	default:
		return fmt.Errorf("不支持的字段类型")
	}
	return nil
}

// RawSection is a generic UCI section, used to read other packages such as
// /etc/config/firewall without a schema.
type RawSection struct {
	Type    string
	Name    string
	Options map[string][]string
}

// ParseRaw parses any UCI file into generic sections.
func ParseRaw(text string) ([]RawSection, error) {
	secs, err := scanUCI(text)
	if err != nil {
		return nil, err
	}
	out := make([]RawSection, 0, len(secs))
	for _, s := range secs {
		r := RawSection{Type: s.typ, Name: s.name, Options: map[string][]string{}}
		for _, o := range s.options {
			r.Options[o.key] = append(r.Options[o.key], o.value)
		}
		out = append(out, r)
	}
	return out, nil
}
