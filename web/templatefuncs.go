package web

// defaultFuncs returns generic helpers available to every loaded template.
//
//   - add(a, b int) int - sum
//   - sub(a, b int) int - difference
//
// Caller FuncMaps override these names.
func defaultFuncs() FuncMap {
	return FuncMap{
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
	}
}
