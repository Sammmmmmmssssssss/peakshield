# Contributing to PeakShield

First off, thank you for considering contributing to PeakShield! It's people like you that make PeakShield such a great tool.

## 1. Where do I go from here?

If you've noticed a bug or have a feature request, make sure to check our [Issues](../../issues) first. If it doesn't exist, feel free to open a new one.

## 2. Setting up your environment

PeakShield has **zero external dependencies**. All you need is Go installed on your machine.

```bash
git clone https://github.com/Sammmmmmmssssssss/peakshield.git
cd peakshield
go build ./...
```

## 3. Testing your changes

Before submitting a pull request, please ensure that all tests pass. PeakShield relies on the standard Go testing framework.

```bash
# Run unit tests and the race detector
go test -v -race -cover ./...

# Run the Go vet tool
go vet ./...
```

If you are modifying hot paths (e.g., `ratelimiter`, `waitingroom`, or `stripper`), please run the benchmarks to ensure no performance regressions or accidental memory allocations were introduced:

```bash
go test -bench=. -benchmem ./...
```

## 4. Code Style & Architecture Constraints

PeakShield is designed to be ultra-fast and lightweight. Please adhere to the following principles when contributing:

1. **Zero Dependencies**: Do not import any external packages outside the Go standard library.
2. **Zero Allocations in Hot Paths**: Avoid unnecessary memory allocations (like creating new maps/slices per request) in `proxy.go` and `waitingroom.go`. Use `sync.Pool` where appropriate.
3. **Lock-Free Concurrency**: Always prefer channels or atomic operations over mutexes. If you must use a mutex, consider sharding it (like the `token_bucket.go` implementation).

## 5. Submitting a Pull Request

1. Fork the repository and create your branch from `main`.
2. Commit your changes. Keep your commits granular and feature-by-feature.
3. Make sure your commit messages are descriptive.
4. Push your branch to GitHub and open a Pull Request.

Thank you for contributing!
