package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Runtime overrides (environment variables and ${VAR} substitution) change the
// in-memory config, but they must never be written back to config.json or
// .security.yml: CLI commands and the web backend load the effective config,
// edit it and save it.
//
// LoadConfig serializes the config just before and just after applying the
// overrides and keeps the leaves that differ. SaveConfig serializes the config
// as usual and, for every such leaf whose value is still the one the override
// produced, puts back the value the files had. A leaf the caller changed on
// purpose keeps the caller's value.

// configOverlay records what runtime overrides changed, per output file.
type configOverlay struct {
	json []leafOverride // paths in config.json
	yaml []leafOverride // paths in .security.yml
}

// leafOverride is one leaf that differs between the file view (before
// overrides) and the effective view (after overrides).
type leafOverride struct {
	path      []string
	file      leafValue
	fileOK    bool // the leaf existed in the file view
	effective leafValue
}

// leafValue is a scalar or an empty container, comparable across encodings.
type leafValue struct {
	kind  string // json: "string", "number", "bool", "null", "{}", "[]"; yaml: node tag, "{}", "[]"
	value string
}

func (o *configOverlay) empty() bool {
	return o == nil || (len(o.json) == 0 && len(o.yaml) == 0)
}

// finishOverlay compares the base captured before overrides with cfg after
// them and stores the differing leaves on cfg.
func finishOverlay(cfg *Config, base overlaySnapshot) error {
	eff, err := captureOverlayLeaves(cfg)
	if err != nil {
		return err
	}
	overlay := &configOverlay{json: diffLeaves(base.json, eff.json, nil)}
	if base.yaml != nil && eff.yaml != nil {
		overlay.yaml = diffLeaves(base.yaml, eff.yaml, secureLeafEqual)
	}
	if overlay.empty() {
		cfg.overlay = nil
		return nil
	}
	cfg.overlay = overlay
	return nil
}

type leafEntry struct {
	path  []string
	value leafValue
}

// overlaySnapshot holds the leaves of config.json and .security.yml. yaml is
// nil when secrets could not be serialized (e.g. an encryption passphrase
// without its SSH key); SaveConfig fails the same way in that case.
type overlaySnapshot struct {
	json map[string]leafEntry
	yaml map[string]leafEntry
}

// captureOverlayLeaves serializes cfg as SaveConfig would and returns the
// leaves of config.json and .security.yml keyed by path.
func captureOverlayLeaves(cfg *Config) (overlaySnapshot, error) {
	var snap overlaySnapshot

	restore := withoutVirtualModels(cfg)
	data, err := marshalConfigJSON(cfg)
	ydata, yerr := marshalSecurityYAML(cfg)
	restore()
	if err != nil {
		return snap, err
	}

	tree, err := parseJSONTree(data)
	if err != nil {
		return snap, err
	}
	snap.json = map[string]leafEntry{}
	flattenJSON(tree, nil, snap.json)

	// Secrets that cannot be serialized leave snap.yaml nil: the YAML side of
	// the overlay is skipped, and SaveConfig would fail the same way.
	if yerr == nil {
		snap.yaml = yamlLeaves(ydata)
	}
	return snap, nil
}

// yamlLeaves flattens a YAML document; it returns nil if data does not parse.
func yamlLeaves(data []byte) map[string]leafEntry {
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil {
		return nil
	}
	leaves := map[string]leafEntry{}
	if len(doc.Content) > 0 {
		flattenYAML(doc.Content[0], nil, leaves)
	}
	return leaves
}

func pathKey(path []string) string { return strings.Join(path, "\x00") }

func diffLeaves(base, eff map[string]leafEntry, equal func(a, b leafValue) bool) []leafOverride {
	if equal == nil {
		equal = func(a, b leafValue) bool { return a == b }
	}
	var out []leafOverride
	for key, e := range eff {
		b, ok := base[key]
		if ok && equal(b.value, e.value) {
			continue
		}
		out = append(out, leafOverride{path: e.path, file: b.value, fileOK: ok, effective: e.value})
	}
	// Deterministic order; array deletions are applied later in reverse index order.
	sort.Slice(out, func(i, j int) bool { return pathKey(out[i].path) < pathKey(out[j].path) })
	return out
}

// secureLeafEqual compares YAML leaves, treating two enc:// values as equal
// when they decrypt to the same plaintext: encryption uses a random nonce, so
// the same secret serializes differently every time.
func secureLeafEqual(a, b leafValue) bool {
	if a == b {
		return true
	}
	if a.kind != b.kind || !strings.HasPrefix(a.value, "enc://") || !strings.HasPrefix(b.value, "enc://") {
		return false
	}
	pa, errA := resolveKey(a.value)
	pb, errB := resolveKey(b.value)
	return errA == nil && errB == nil && pa == pb
}

// ── config.json side ────────────────────────────────────────────────────────

func marshalConfigJSON(cfg *Config) ([]byte, error) {
	return json.MarshalIndent(cfg, "", "  ")
}

// restoreConfigJSON returns data (config.json as SaveConfig would write it)
// with override-produced values replaced by their file values.
func restoreConfigJSON(data []byte, overrides []leafOverride) ([]byte, error) {
	if len(overrides) == 0 {
		return data, nil
	}
	tree, err := parseJSONTree(data)
	if err != nil {
		return nil, err
	}
	var deletions []jsonDeletion
	for _, o := range overrides {
		parent, last, node := tree.lookup(o.path)
		if node == nil || node.leaf() != o.effective {
			continue // caller changed it (or removed it): keep the caller's value
		}
		if o.fileOK {
			*node = *jsonNodeFromLeaf(o.file)
			continue
		}
		deletions = append(deletions, jsonDeletion{parent: parent, key: last})
	}
	applyJSONDeletions(deletions)
	var buf bytes.Buffer
	tree.encode(&buf, 0)
	return buf.Bytes(), nil
}

// jnode is an order-preserving JSON value, so a restored config.json keeps
// exactly the layout json.MarshalIndent produced.
type jnode struct {
	kind  string // "object", "array", "string", "number", "bool", "null"
	keys  []string
	vals  []*jnode // object values, or array items
	value string   // string value, number text, or "true"/"false"
}

func parseJSONTree(data []byte) (*jnode, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	n, err := parseJSONValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected trailing data in config JSON")
	}
	return n, nil
}

func parseJSONValue(dec *json.Decoder) (*jnode, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			n := &jnode{kind: "object"}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("invalid object key %v", kt)
				}
				v, err := parseJSONValue(dec)
				if err != nil {
					return nil, err
				}
				n.keys = append(n.keys, key)
				n.vals = append(n.vals, v)
			}
			_, err := dec.Token() // '}'
			return n, err
		case '[':
			n := &jnode{kind: "array"}
			for dec.More() {
				v, err := parseJSONValue(dec)
				if err != nil {
					return nil, err
				}
				n.vals = append(n.vals, v)
			}
			_, err := dec.Token() // ']'
			return n, err
		}
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	case string:
		return &jnode{kind: "string", value: t}, nil
	case json.Number:
		return &jnode{kind: "number", value: t.String()}, nil
	case bool:
		return &jnode{kind: "bool", value: strconv.FormatBool(t)}, nil
	case nil:
		return &jnode{kind: "null"}, nil
	}
	return nil, fmt.Errorf("unexpected JSON token %v", tok)
}

func (n *jnode) leaf() leafValue {
	switch n.kind {
	case "object":
		return leafValue{kind: "{}"}
	case "array":
		return leafValue{kind: "[]"}
	}
	return leafValue{kind: n.kind, value: n.value}
}

func jsonNodeFromLeaf(l leafValue) *jnode {
	switch l.kind {
	case "{}":
		return &jnode{kind: "object"}
	case "[]":
		return &jnode{kind: "array"}
	}
	return &jnode{kind: l.kind, value: l.value}
}

func flattenJSON(n *jnode, path []string, out map[string]leafEntry) {
	if (n.kind == "object" || n.kind == "array") && len(n.vals) > 0 {
		for i, v := range n.vals {
			seg := "#" + strconv.Itoa(i)
			if n.kind == "object" {
				seg = n.keys[i]
			}
			flattenJSON(v, appendPath(path, seg), out)
		}
		return
	}
	out[pathKey(path)] = leafEntry{path: path, value: n.leaf()}
}

func appendPath(path []string, seg string) []string {
	out := make([]string, len(path)+1)
	copy(out, path)
	out[len(path)] = seg
	return out
}

// lookup returns the node at path plus its parent and final segment.
func (n *jnode) lookup(path []string) (parent *jnode, last string, node *jnode) {
	node = n
	for _, seg := range path {
		parent, last = node, seg
		node = node.child(seg)
		if node == nil {
			return nil, "", nil
		}
	}
	return parent, last, node
}

func (n *jnode) child(seg string) *jnode {
	switch n.kind {
	case "object":
		for i, k := range n.keys {
			if k == seg {
				return n.vals[i]
			}
		}
	case "array":
		if i, ok := arrayIndex(seg); ok && i < len(n.vals) {
			return n.vals[i]
		}
	}
	return nil
}

func arrayIndex(seg string) (int, bool) {
	if !strings.HasPrefix(seg, "#") {
		return 0, false
	}
	i, err := strconv.Atoi(seg[1:])
	return i, err == nil && i >= 0
}

type jsonDeletion struct {
	parent *jnode
	key    string
}

// applyJSONDeletions removes object keys and array items. Array items are
// removed from the highest index down so earlier indexes stay valid.
func applyJSONDeletions(dels []jsonDeletion) {
	sort.SliceStable(dels, func(i, j int) bool {
		ii, _ := arrayIndex(dels[i].key)
		jj, _ := arrayIndex(dels[j].key)
		return ii > jj
	})
	for _, d := range dels {
		p := d.parent
		if p == nil {
			continue
		}
		switch p.kind {
		case "object":
			for i, k := range p.keys {
				if k == d.key {
					p.keys = append(p.keys[:i], p.keys[i+1:]...)
					p.vals = append(p.vals[:i], p.vals[i+1:]...)
					break
				}
			}
		case "array":
			if i, ok := arrayIndex(d.key); ok && i < len(p.vals) {
				p.vals = append(p.vals[:i], p.vals[i+1:]...)
			}
		}
	}
}

// encode writes n in the same layout as json.MarshalIndent(v, "", "  ").
func (n *jnode) encode(buf *bytes.Buffer, depth int) {
	switch n.kind {
	case "object", "array":
		open, closing := byte('{'), byte('}')
		if n.kind == "array" {
			open, closing = '[', ']'
		}
		buf.WriteByte(open)
		if len(n.vals) == 0 {
			buf.WriteByte(closing)
			return
		}
		for i, v := range n.vals {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.WriteByte('\n')
			buf.WriteString(strings.Repeat("  ", depth+1))
			if n.kind == "object" {
				writeJSONString(buf, n.keys[i])
				buf.WriteString(": ")
			}
			v.encode(buf, depth+1)
		}
		buf.WriteByte('\n')
		buf.WriteString(strings.Repeat("  ", depth))
		buf.WriteByte(closing)
	case "string":
		writeJSONString(buf, n.value)
	case "null":
		buf.WriteString("null")
	default: // number, bool
		buf.WriteString(n.value)
	}
}

func writeJSONString(buf *bytes.Buffer, s string) {
	b, _ := json.Marshal(s) // same escaping (including HTML) as MarshalIndent
	buf.Write(b)
}

// ── .security.yml side ──────────────────────────────────────────────────────

func marshalSecurityYAML(cfg *Config) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func yamlLeaf(n *yaml.Node) leafValue {
	switch n.Kind {
	case yaml.MappingNode:
		return leafValue{kind: "{}"}
	case yaml.SequenceNode:
		return leafValue{kind: "[]"}
	}
	return leafValue{kind: n.ShortTag(), value: n.Value}
}

func flattenYAML(n *yaml.Node, path []string, out map[string]leafEntry) {
	switch n.Kind {
	case yaml.MappingNode:
		if len(n.Content) > 0 {
			for i := 0; i+1 < len(n.Content); i += 2 {
				flattenYAML(n.Content[i+1], appendPath(path, n.Content[i].Value), out)
			}
			return
		}
	case yaml.SequenceNode:
		if len(n.Content) > 0 {
			for i, c := range n.Content {
				flattenYAML(c, appendPath(path, "#"+strconv.Itoa(i)), out)
			}
			return
		}
	}
	out[pathKey(path)] = leafEntry{path: path, value: yamlLeaf(n)}
}

func yamlChild(n *yaml.Node, seg string) (*yaml.Node, int) {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == seg {
				return n.Content[i+1], i
			}
		}
	case yaml.SequenceNode:
		if i, ok := arrayIndex(seg); ok && i < len(n.Content) {
			return n.Content[i], i
		}
	}
	return nil, -1
}

// restoreSecurityYAML returns data (.security.yml as SaveConfig would write it)
// with override-produced values replaced by their file values.
func restoreSecurityYAML(data []byte, overrides []leafOverride) ([]byte, error) {
	if len(overrides) == 0 {
		return data, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return data, nil
	}
	root := doc.Content[0]

	type deletion struct {
		parent *yaml.Node
		index  int
	}
	var dels []deletion
	for _, o := range overrides {
		var parent *yaml.Node
		node, index := root, -1
		for _, seg := range o.path {
			parent = node
			node, index = yamlChild(node, seg)
			if node == nil {
				break
			}
		}
		if node == nil || !secureLeafEqual(yamlLeaf(node), o.effective) {
			continue
		}
		if o.fileOK {
			restoreYAMLLeaf(node, o.file)
			continue
		}
		dels = append(dels, deletion{parent: parent, index: index})
	}
	sort.SliceStable(dels, func(i, j int) bool { return dels[i].index > dels[j].index })
	for _, d := range dels {
		if d.parent.Kind == yaml.MappingNode {
			d.parent.Content = append(d.parent.Content[:d.index], d.parent.Content[d.index+2:]...)
		} else {
			d.parent.Content = append(d.parent.Content[:d.index], d.parent.Content[d.index+1:]...)
		}
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func restoreYAMLLeaf(node *yaml.Node, l leafValue) {
	switch l.kind {
	case "{}":
		*node = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Style: yaml.FlowStyle}
	case "[]":
		*node = yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
	default:
		*node = yaml.Node{Kind: yaml.ScalarNode, Tag: l.kind, Value: l.value}
	}
}
