package plan

import (
	"encoding/json"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

func supportedType(t string) bool {
	switch t {
	case "aws_db_instance", "aws_rds_cluster", "aws_security_group", "aws_security_group_rule", "aws_vpc_security_group_ingress_rule", "aws_route", "aws_route_table", "aws_network_acl", "aws_network_acl_rule":
		return true
	}
	return false
}
func field(r Resource, key string, before bool) Value {
	for _, p := range r.Paths {
		if len(p.Path) == 0 {
			if before {
				return p.Before
			}
			return p.After
		}
		if len(p.Path) == 1 && p.Path[0] == key {
			if before {
				return p.Before
			}
			return p.After
		}
	}
	return Value{State: "absent"}
}
func uncertain(v Value) bool { return v.State == "hidden" || v.State == "unknown" }
func uncertainTree(v Value) bool {
	if uncertain(v) {
		return true
	}
	if m, ok := v.Value.(map[string]Value); ok {
		for _, v := range m {
			if uncertainTree(v) {
				return true
			}
		}
	}
	if a, ok := v.Value.([]Value); ok {
		for _, v := range a {
			if uncertainTree(v) {
				return true
			}
		}
	}
	return false
}
func get(v Value, key string) Value {
	if uncertain(v) {
		return v
	}
	m, _ := v.Value.(map[string]Value)
	if v, ok := m[key]; ok {
		return v
	}
	return Value{State: "absent"}
}
func known(v Value, typ string) bool { return v.State == "known" && v.Type == typ }
func valueString(v Value) (string, bool) {
	s, ok := v.Value.(string)
	return s, ok && known(v, "string")
}
func integer(v Value) (int, bool) {
	s, ok := v.Value.(string)
	if !ok || !known(v, "number") {
		return 0, false
	}
	i, e := strconv.Atoi(s)
	return i, e == nil
}
func boolean(v Value) (bool, bool) { b, ok := v.Value.(bool); return b, ok && known(v, "boolean") }
func equal(a, b Value) bool        { return !uncertainTree(a) && !uncertainTree(b) && digest(a) == digest(b) }
func addFinding(r *Resource, rule, severity, message, coverage string, path []any, args map[string]any) {
	if path == nil {
		path = []any{}
	}
	if args == nil {
		args = map[string]any{}
	}
	f := Finding{Rule: rule, Severity: severity, ResourceID: r.ID, Path: path, Message: message, Arguments: args, Coverage: coverage}
	// All parts are static profile facts, an opaque supplied ID and safe path.
	f.ID = digest([]any{RuleVersion, r.ID, rule, path})
	r.Findings = append(r.Findings, f)
}
func classify(r *Resource, semantic bool) error {
	destructive := "high"
	if semantic && (r.Type == "aws_db_instance" || r.Type == "aws_rds_cluster") {
		destructive = "critical"
	}
	if len(r.Actions) == 2 {
		addFinding(r, "CF-ACTION-REPLACE", destructive, "RESOURCE_REPLACED", "supported", nil, map[string]any{"replacement_order": r.ReplacementOrder, "replacement_paths": r.ReplacementPaths})
	} else if len(r.Actions) == 1 && r.Actions[0] == "delete" {
		addFinding(r, "CF-ACTION-DELETE", destructive, "RESOURCE_DELETED", "supported", nil, nil)
	}
	if !semantic {
		return nil
	}
	switch r.Type {
	case "aws_db_instance", "aws_rds_cluster":
		if e := rdsRules(r); e != nil {
			return e
		}
	case "aws_security_group", "aws_security_group_rule", "aws_vpc_security_group_ingress_rule":
		if e := ingressRules(r); e != nil {
			return e
		}
	case "aws_route", "aws_route_table":
		if e := routeRules(r); e != nil {
			return e
		}
	case "aws_network_acl", "aws_network_acl_rule":
		if e := aclRules(r); e != nil {
			return e
		}
	}
	sortFindings(r.Findings, []Resource{*r})
	return nil
}
func rdsRules(r *Resource) error {
	if len(r.Actions) == 1 && r.Actions[0] != "update" {
		return nil
	}
	for _, item := range []struct{ key, rule, typ string }{{"deletion_protection", "CF-RDS-PROTECTION", "boolean"}, {"backup_retention_period", "CF-RDS-BACKUP", "number"}, {"publicly_accessible", "CF-RDS-PUBLIC", "boolean"}} {
		if r.Type == "aws_rds_cluster" && item.key == "publicly_accessible" {
			continue
		}
		b, a := field(*r, item.key, true), field(*r, item.key, false)
		if uncertain(b) || uncertain(a) {
			addFinding(r, item.rule, "attention", "SAFETY_SETTING_UNCERTAIN", "uncertain", []any{item.key}, nil)
			continue
		}
		if !known(b, item.typ) || !known(a, item.typ) {
			continue
		}
		risky := false
		severity := "critical"
		switch item.key {
		case "deletion_protection":
			bb, _ := boolean(b)
			ab, _ := boolean(a)
			risky = bb && !ab
		case "publicly_accessible":
			bb, _ := boolean(b)
			ab, _ := boolean(a)
			risky = !bb && ab
			severity = "high"
		case "backup_retention_period":
			bb, bok := integer(b)
			ab, aok := integer(a)
			if !bok || !aok || bb < 0 || ab < 0 {
				return ErrUnsupported
			}
			risky = ab < bb
		}
		if risky {
			addFinding(r, item.rule, severity, "SAFETY_SETTING_WEAKENED", "supported", []any{item.key}, nil)
		}
	}
	return nil
}

// An atom is a canonical peer plus protocol and inclusive port/ICMP range. It
// describes settings, never resolved SG/prefix-list topology or reachability.
type atom struct {
	protocol, kind, peer string
	lo, hi               int
}

func atomKey(a atom) string { b, _ := json.Marshal(a.key()); return string(b) }
func (a atom) key() []any   { return []any{a.kind, a.peer, a.protocol, a.lo, a.hi} }
func protocol(v Value) (string, bool, error) {
	if uncertain(v) {
		return "", true, nil
	}
	s, ok := valueString(v)
	if !ok {
		return "", false, ErrUnsupported
	}
	switch strings.ToLower(s) {
	case "tcp":
		return "6", false, nil
	case "udp":
		return "17", false, nil
	case "icmp":
		return "1", false, nil
	case "icmpv6", "ipv6-icmp":
		return "58", false, nil
	case "-1":
		return "all", false, nil
	}
	n, e := strconv.Atoi(s)
	if e != nil || n < 0 || n > 255 {
		return "", false, ErrUnsupported
	}
	return strconv.Itoa(n), false, nil
}
func atomPorts(proto string, from, to Value) (int, int, bool, error) {
	if proto == "all" {
		return 0, 0, false, nil
	}
	if proto != "6" && proto != "17" && proto != "1" && proto != "58" {
		return 0, 0, false, nil
	}
	if uncertain(from) || uncertain(to) {
		return 0, 0, true, nil
	}
	lo, ok := integer(from)
	hi, ok2 := integer(to)
	if !ok || !ok2 {
		return 0, 0, false, ErrUnsupported
	}
	if proto == "1" || proto == "58" {
		if lo < -1 || lo > 255 || hi < -1 || hi > 255 {
			return 0, 0, false, ErrUnsupported
		}
	} else if lo < 0 || hi > 65535 || hi < lo {
		return 0, 0, false, ErrUnsupported
	}
	return lo, hi, false, nil
}
func cidr(s string, family int) (string, error) {
	p, e := netip.ParsePrefix(s)
	if e != nil || p.Addr().Is4In6() || family == 4 && !p.Addr().Is4() || family == 6 && !p.Addr().Is6() {
		return "", ErrUnsupported
	}
	return p.Masked().String(), nil
}
func collection(v Value) ([]Value, bool, error) {
	if uncertain(v) {
		return nil, true, nil
	}
	if v.State == "null" {
		return []Value{}, false, nil
	}
	if !known(v, "array") {
		return nil, false, ErrUnsupported
	}
	a, ok := v.Value.([]Value)
	if !ok || len(a) > 4096 {
		return nil, false, ErrUnsupported
	}
	return a, false, nil
}
func active(r Resource, before bool) bool {
	if len(r.Actions) == 1 {
		return !(before && r.Actions[0] == "create" || !before && r.Actions[0] == "delete")
	}
	return true
}
func rootValue(r Resource, before bool) Value {
	if len(r.Paths) == 1 && len(r.Paths[0].Path) == 0 {
		if before {
			return r.Paths[0].Before
		}
		return r.Paths[0].After
	}
	m := map[string]Value{}
	for _, p := range r.Paths {
		if len(p.Path) == 1 {
			if before {
				m[p.Path[0].(string)] = p.Before
			} else {
				m[p.Path[0].(string)] = p.After
			}
		}
	}
	return Value{"known", "object", m}
}
func ingressAtoms(r Resource, before bool) ([]atom, bool, error) {
	if !active(r, before) {
		return []atom{}, false, nil
	}
	var entries []Value
	unc := false
	root := rootValue(r, before)
	if uncertain(root) {
		return nil, true, nil
	}
	if r.Type == "aws_security_group" {
		a, u, e := collection(get(root, "ingress"))
		if e != nil {
			return nil, false, e
		}
		entries = a
		unc = u
	} else {
		entries = []Value{root}
	}
	atoms := []atom{}
	for _, entry := range entries {
		if uncertain(entry) {
			unc = true
			continue
		}
		if !known(entry, "object") {
			return nil, false, ErrUnsupported
		}
		if r.Type == "aws_security_group_rule" {
			typ := get(entry, "type")
			if uncertain(typ) {
				unc = true
				continue
			}
			s, ok := valueString(typ)
			if !ok || (s != "ingress" && s != "egress") {
				return nil, false, ErrUnsupported
			}
			if s == "egress" {
				continue
			}
		}
		protocolKey := "protocol"
		if r.Type == "aws_vpc_security_group_ingress_rule" {
			protocolKey = "ip_protocol"
		}
		p, u, e := protocol(get(entry, protocolKey))
		if e != nil {
			return nil, false, e
		}
		if u {
			unc = true
			continue
		}
		lo, hi, u, e := atomPorts(p, get(entry, "from_port"), get(entry, "to_port"))
		if e != nil {
			return nil, false, e
		}
		if u {
			unc = true
			continue
		}
		peers := []atom{}
		addPeer := func(v Value, kind string, family int) error {
			if uncertain(v) {
				unc = true
				return nil
			}
			if v.State == "null" {
				return nil
			}
			s, ok := valueString(v)
			if !ok {
				return ErrUnsupported
			}
			if s == "" {
				return nil
			}
			if family != 0 {
				var e error
				s, e = cidr(s, family)
				if e != nil {
					return e
				}
			}
			peers = append(peers, atom{p, kind, s, lo, hi})
			if len(atoms)+len(peers) > 4096 {
				return ErrUnsupported
			}
			return nil
		}
		if r.Type == "aws_vpc_security_group_ingress_rule" {
			for _, f := range []struct {
				k, kind string
				family  int
			}{{"cidr_ipv4", "cidr", 4}, {"cidr_ipv6", "cidr", 6}, {"prefix_list_id", "prefix", 0}, {"referenced_security_group_id", "sg", 0}} {
				v := get(entry, f.k)
				if v.State == "absent" {
					continue
				}
				if e = addPeer(v, f.kind, f.family); e != nil {
					return nil, false, e
				}
			}
			if len(peers) > 1 {
				return nil, false, ErrUnsupported
			}
			if len(peers) == 0 && !unc {
				return nil, false, ErrUnsupported
			}
		} else {
			for _, f := range []struct {
				k, kind string
				family  int
			}{{"cidr_blocks", "cidr", 4}, {"ipv6_cidr_blocks", "cidr", 6}, {"prefix_list_ids", "prefix", 0}, {"security_groups", "sg", 0}} {
				if f.k == "security_groups" && r.Type == "aws_security_group_rule" {
					continue
				}
				v := get(entry, f.k)
				a, u, e := collection(v)
				if e != nil {
					return nil, false, e
				}
				unc = unc || u
				for _, v := range a {
					if e = addPeer(v, f.kind, f.family); e != nil {
						return nil, false, e
					}
				}
			}
			if r.Type == "aws_security_group_rule" {
				v := get(entry, "source_security_group_id")
				if v.State != "absent" {
					if e = addPeer(v, "sg", 0); e != nil {
						return nil, false, e
					}
				}
			}
			self := get(entry, "self")
			if uncertain(self) {
				unc = true
			} else if b, ok := boolean(self); ok {
				if b {
					peers = append(peers, atom{p, "self", "self", lo, hi})
				}
			} else if self.State != "null" {
				return nil, false, ErrUnsupported
			}
		}
		atoms = append(atoms, peers...)
		if len(atoms) > 4096 {
			return nil, false, ErrUnsupported
		}
	}
	return mergeAtoms(atoms), unc, nil
}
func mergeAtoms(a []atom) []atom {
	sort.Slice(a, func(i, j int) bool {
		x, y := a[i], a[j]
		if x.kind != y.kind {
			return x.kind < y.kind
		}
		if x.peer != y.peer {
			return x.peer < y.peer
		}
		if x.protocol != y.protocol {
			return x.protocol < y.protocol
		}
		if x.lo != y.lo {
			return x.lo < y.lo
		}
		return x.hi < y.hi
	})
	out := []atom{}
	for _, v := range a {
		if len(out) > 0 {
			p := &out[len(out)-1]
			if *p == v {
				continue
			}
			if (v.protocol == "6" || v.protocol == "17") && p.kind == v.kind && p.peer == v.peer && p.protocol == v.protocol && v.lo <= p.hi+1 {
				p.hi = max(p.hi, v.hi)
				continue
			}
		}
		out = append(out, v)
	}
	return out
}
func peerCovers(old, new atom) bool {
	if old.kind != new.kind {
		return false
	}
	if old.kind != "cidr" {
		return old.peer == new.peer
	}
	a, e := netip.ParsePrefix(old.peer)
	b, e2 := netip.ParsePrefix(new.peer)
	return e == nil && e2 == nil && a.Addr().BitLen() == b.Addr().BitLen() && a.Bits() <= b.Bits() && a.Contains(b.Addr())
}
func covers(old, new atom) bool {
	if !peerCovers(old, new) {
		return false
	}
	if old.protocol == "all" {
		return true
	}
	if old.protocol != new.protocol {
		return false
	}
	switch old.protocol {
	case "6", "17":
		return old.lo <= new.lo && old.hi >= new.hi
	case "1", "58":
		return (old.lo == -1 || old.lo == new.lo) && (old.hi == -1 || old.hi == new.hi)
	}
	return true
}
func uncovered(a, b []atom) bool {
	for _, n := range a {
		covered := false
		for _, o := range b {
			if covers(o, n) {
				covered = true
				break
			}
		}
		if !covered {
			return true
		}
	}
	return false
}
func internetWide(atoms []atom) []string {
	ipv4, ipv6 := false, false
	for _, a := range atoms {
		if a.kind != "cidr" {
			continue
		}
		if a.peer == "0.0.0.0/0" {
			ipv4 = true
		}
		if a.peer == "::/0" {
			ipv6 = true
		}
	}
	out := []string{}
	if ipv4 {
		out = append(out, "ipv4")
	}
	if ipv6 {
		out = append(out, "ipv6")
	}
	return out
}
func ingressRules(r *Resource) error {
	b, bu, e := ingressAtoms(*r, true)
	if e != nil {
		return e
	}
	a, au, e := ingressAtoms(*r, false)
	if e != nil {
		return e
	}
	if len(r.Actions) == 1 && (r.Actions[0] == "no-op" || r.Actions[0] == "read") {
		return nil
	}

	if bu || au {
		addFinding(r, "CF-SG-INGRESS", "high", "INGRESS_COVERAGE_UNCERTAIN", "uncertain", []any{"ingress"}, nil)
		return nil
	}
	// Two separate summarized categories have distinct canonical safe paths and
	// IDs; no expanded peer, array index or secret is included in an identifier.
	if uncovered(a, b) {
		addFinding(r, "CF-SG-INGRESS", "high", "INGRESS_ADDED_OR_POTENTIALLY_WIDENED", "supported", []any{"ingress", "$added"}, map[string]any{"comparison": "single_cover", "description": "added_or_potentially_widened_or_regrouped", "internet_wide": internetWide(a)})
	}
	if uncovered(b, a) {
		addFinding(r, "CF-SG-INGRESS", "attention", "INGRESS_REMOVED_OR_POTENTIALLY_TIGHTENED", "supported", []any{"ingress", "$removed"}, map[string]any{"comparison": "single_cover", "internet_wide": internetWide(b)})
	}
	return nil
}

// Routes are paired by destination, and ACLs by direction/rule number. Only
// safe structural categories enter findings; canonical tuples remain private.
type route struct{ destination, target string }

func routes(r Resource, before bool) (map[string]route, bool, error) {
	out := map[string]route{}
	if !active(r, before) {
		return out, false, nil
	}
	root := rootValue(r, before)
	if uncertain(root) {
		return out, true, nil
	}
	entries := []Value{root}
	unc := false
	inline := r.Type == "aws_route_table"
	if inline {
		a, u, e := collection(get(root, "route"))
		if e != nil {
			return nil, false, e
		}
		entries = a
		unc = u
	}
	for _, entry := range entries {
		if uncertain(entry) {
			unc = true
			continue
		}
		if !known(entry, "object") {
			return nil, false, ErrUnsupported
		}
		dests := []struct {
			k, kind string
			family  int
		}{{"destination_cidr_block", "ipv4", 4}, {"destination_ipv6_cidr_block", "ipv6", 6}, {"destination_prefix_list_id", "prefix", 0}}
		if inline {
			dests[0].k = "cidr_block"
			dests[1].k = "ipv6_cidr_block"
		}
		dest, du, e := selected(entry, dests)
		if e != nil {
			return nil, false, e
		}
		targets := []struct {
			k, kind string
			family  int
		}{}
		for _, k := range []string{"gateway_id", "nat_gateway_id", "network_interface_id", "transit_gateway_id", "vpc_peering_connection_id", "egress_only_gateway_id", "local_gateway_id", "carrier_gateway_id", "core_network_arn", "vpc_endpoint_id"} {
			targets = append(targets, struct {
				k, kind string
				family  int
			}{k, k, 0})
		}
		if !inline {
			targets = append(targets, struct {
				k, kind string
				family  int
			}{"instance_id", "instance_id", 0})
		}
		target, tu, e := selected(entry, targets)
		if e != nil {
			return nil, false, e
		}
		if du || tu {
			unc = true
			continue
		}
		v := route{dest, target}
		if old, ok := out[dest]; ok && old != v {
			return nil, false, ErrUnsupported
		}
		out[dest] = v
	}
	return out, unc, nil
}
func selected(entry Value, fields []struct {
	k, kind string
	family  int
}) (string, bool, error) {
	values := []string{}
	unc := false
	for _, f := range fields {
		v := get(entry, f.k)
		if uncertain(v) {
			unc = true
			continue
		}
		if v.State == "null" || v.State == "absent" {
			continue
		}
		s, ok := valueString(v)
		if !ok {
			return "", false, ErrUnsupported
		}
		if s == "" {
			continue
		}
		if f.family != 0 {
			var e error
			s, e = cidr(s, f.family)
			if e != nil {
				return "", false, e
			}
		}
		values = append(values, f.kind+":"+s)
	}
	if len(values) > 1 {
		return "", false, ErrUnsupported
	}
	if unc {
		return "", true, nil
	}
	if len(values) != 1 {
		return "", false, ErrUnsupported
	}
	return values[0], false, nil
}
func routeRules(r *Resource) error {
	b, bu, e := routes(*r, true)
	if e != nil {
		return e
	}
	a, au, e := routes(*r, false)
	if e != nil {
		return e
	}
	if len(r.Actions) == 1 && (r.Actions[0] == "no-op" || r.Actions[0] == "read") {
		return nil
	}

	path := []any{}
	if r.Type == "aws_route_table" {
		path = []any{"route"}
	}
	if bu || au {
		addFinding(r, "CF-VPC-ROUTE", "high", "ROUTE_COVERAGE_UNCERTAIN", "uncertain", path, nil)
		return nil
	}
	added, removed, changed := 0, 0, 0
	for k, v := range b {
		if n, ok := a[k]; !ok {
			removed++
		} else if n.target != v.target {
			changed++
		}
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			added++
		}
	}
	if added+removed+changed > 0 {
		message := "ROUTE_CHANGED"
		if changed > 0 && added+removed == 0 {
			message = "ROUTE_TARGET_CHANGED"
		} else if added > 0 && removed+changed == 0 {
			message = "ROUTE_ADDED"
		} else if removed > 0 && added+changed == 0 {
			message = "ROUTE_REMOVED"
		}
		addFinding(r, "CF-VPC-ROUTE", "high", message, "supported", path, map[string]any{"added": added, "removed": removed, "target_changed": changed, "consequence": "potential_connectivity_impact"})
	}
	return nil
}

type aclKey struct {
	egress bool
	number int
}
type aclTuple struct {
	action, protocol, cidr string
	lo, hi                 int
}

func aclEntries(r Resource, before bool) (map[aclKey]aclTuple, bool, error) {
	out := map[aclKey]aclTuple{}
	if !active(r, before) {
		return out, false, nil
	}
	root := rootValue(r, before)
	if uncertain(root) {
		return out, true, nil
	}
	unc := false
	type entry struct {
		v         Value
		direction bool
	}
	entries := []entry{}
	if r.Type == "aws_network_acl" {
		for _, direction := range []bool{false, true} {
			key := "ingress"
			if direction {
				key = "egress"
			}
			a, u, e := collection(get(root, key))
			if e != nil {
				return nil, false, e
			}
			unc = unc || u
			for _, v := range a {
				entries = append(entries, entry{v, direction})
			}
		}
	} else {
		entries = append(entries, entry{root, false})
	}
	if len(entries) > 4096 {
		return nil, false, ErrUnsupported
	}
	for _, entry := range entries {
		v := entry.v
		if uncertain(v) {
			unc = true
			continue
		}
		if !known(v, "object") {
			return nil, false, ErrUnsupported
		}
		direction := entry.direction
		numberKey, actionKey := "rule_no", "action"
		if r.Type == "aws_network_acl_rule" {
			numberKey = "rule_number"
			actionKey = "rule_action"
			d := get(v, "egress")
			if uncertain(d) {
				unc = true
				continue
			}
			var ok bool
			direction, ok = boolean(d)
			if !ok {
				return nil, false, ErrUnsupported
			}
		}
		number := get(v, numberKey)
		if uncertain(number) {
			unc = true
			continue
		}
		num, ok := integer(number)
		if !ok || num < 1 || num > 32766 {
			return nil, false, ErrUnsupported
		}
		action := get(v, actionKey)
		if uncertain(action) {
			unc = true
			continue
		}
		act, ok := valueString(action)
		if !ok || (act != "allow" && act != "deny") {
			return nil, false, ErrUnsupported
		}
		proto, u, e := protocol(get(v, "protocol"))
		if e != nil {
			return nil, false, e
		}
		if u {
			unc = true
			continue
		}
		from, to := get(v, "from_port"), get(v, "to_port")
		if proto == "1" || proto == "58" {
			from, to = get(v, "icmp_type"), get(v, "icmp_code")
		}
		lo, hi, u, e := atomPorts(proto, from, to)
		if e != nil {
			return nil, false, e
		}
		if u {
			unc = true
			continue
		}
		dest, du, e := selected(v, []struct {
			k, kind string
			family  int
		}{{"cidr_block", "ipv4", 4}, {"ipv6_cidr_block", "ipv6", 6}})
		if e != nil {
			return nil, false, e
		}
		if du {
			unc = true
			continue
		}
		key := aclKey{direction, num}
		tuple := aclTuple{act, proto, dest, lo, hi}
		if old, ok := out[key]; ok && old != tuple {
			return nil, false, ErrUnsupported
		}
		out[key] = tuple
	}
	return out, unc, nil
}
func aclRules(r *Resource) error {
	b, bu, e := aclEntries(*r, true)
	if e != nil {
		return e
	}
	a, au, e := aclEntries(*r, false)
	if e != nil {
		return e
	}
	if len(r.Actions) == 1 && (r.Actions[0] == "no-op" || r.Actions[0] == "read") {
		return nil
	}

	if bu || au {
		addFinding(r, "CF-NACL-RULE", "high", "ACL_COVERAGE_UNCERTAIN", "uncertain", []any{}, nil)
		return nil
	}
	all := map[aclKey]bool{}
	for k := range b {
		all[k] = true
	}
	for k := range a {
		all[k] = true
	}
	ks := []aclKey{}
	for k := range all {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool {
		if ks[i].egress != ks[j].egress {
			return !ks[i].egress
		}
		return ks[i].number < ks[j].number
	})
	changes := []any{}
	for _, k := range ks {
		bv, bok := b[k]
		av, aok := a[k]
		kind := "changed"
		if !bok {
			kind = "added"
		} else if !aok {
			kind = "removed"
		} else if bv == av {
			continue
		}
		changes = append(changes, map[string]any{"egress": k.egress, "rule_number": k.number, "change": kind})
	}
	if len(changes) > 0 {
		addFinding(r, "CF-NACL-RULE", "high", "ACL_POTENTIAL_ACCESS_IMPACT", "supported", []any{}, map[string]any{"changes": changes})
	}
	return nil
}
