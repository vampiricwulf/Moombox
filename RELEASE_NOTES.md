### Internal

- **Pre-download Go modules in CI** to avoid the parallel build race that fetched every dependency three times. The v2.6.6 release pipeline rewrite parallelized cross-compilation but didn't account for `go build`'s lack of inter-process synchronization on the module cache — each of the 3 concurrent builds raced and re-downloaded chi, goja, isatty, etc. independently. Single `go mod download` before the parallel builds serializes the network fetch; the three builds then hit warm cache and only do compile+link work.
