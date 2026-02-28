**Improvements**

- Static asset cache busting: CSS/JS files now use `no-cache` headers and `index.html` references include `?v=<commit>` query strings, eliminating the need to hard-refresh after updates
