### Improvements

- Superseded launcher binary (`.exe~`) is now automatically deleted on exit via a deferred cleanup process, instead of waiting until the next launch
- Renamed leftover binary suffix from `.super` to `.exe~` to match Go convention
