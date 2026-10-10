# greeter

A tiny package that builds greeting messages.

## Usage

```go
import "example.com/greeter-docs"

fmt.Println(greeter.Greet("Ada"))             // Hello, Ada!
fmt.Println(greeter.Greet(""))                // Hello, world!
fmt.Println(greeter.GreetAll([]string{"a"}))  // [Hello, a!]
```

`Greet` falls back to "world" when the name is empty. `GreetAll` returns one
greeting per name, in the same order.

## Testing

```
go test ./...
```
