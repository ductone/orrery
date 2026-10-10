// Package greeter builds greeting messages.
package greeter

// Greet returns a greeting for name. An empty name greets "world".
func Greet(name string) string {
	if name == "" {
		name = "world"
	}
	return "Hello, " + name + "!"
}

// GreetAll returns one greeting per name, in order.
func GreetAll(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, Greet(n))
	}
	return out
}
