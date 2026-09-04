package policy

// mergePatch applies patch to target following RFC 7386 (JSON Merge Patch):
// objects merge recursively, arrays and scalars replace, a null in the patch
// deletes the key. target is never modified; the result shares no maps with
// it, so a snapshot built from it cannot be mutated by a later patch.
func mergePatch(target, patch any) any {
	pm, ok := patch.(map[string]any)
	if !ok {
		return cloneValue(patch)
	}
	out := map[string]any{}
	if tm, ok := target.(map[string]any); ok {
		for k, v := range tm {
			out[k] = cloneValue(v)
		}
	}
	for k, v := range pm {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = mergePatch(out[k], v)
	}
	return out
}

// stripNulls returns v with every null-valued object key removed,
// recursively. Used before filling defaults: in the global document a null
// and an absent field mean the same thing - "use the default" - whereas in
// an override's patch a null is an instruction (delete), which mergePatch
// handles before this runs.
func stripNulls(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return cloneValue(v)
	}
	out := make(map[string]any, len(m))
	for k, val := range m {
		if val == nil {
			continue
		}
		out[k] = stripNulls(val)
	}
	return out
}

// cloneValue deep-copies the generic JSON value v.
func cloneValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = cloneValue(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = cloneValue(val)
		}
		return out
	default:
		return v
	}
}
