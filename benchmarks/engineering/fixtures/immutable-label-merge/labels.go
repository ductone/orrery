package immutablelabelmerge

// MergeLabels returns base with overlay values taking precedence.
func MergeLabels(base, overlay map[string]string) map[string]string {
	for key, value := range overlay {
		base[key] = value
	}
	return base
}
