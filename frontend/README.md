# frontend/

React 19 + TypeScript 7 + Vite 8 + Tailwind 4 SPA for Dubarr.

```sh
npm install
npm run dev     # dev server
npm run build   # production build -> ../backend/web (embedded by the Go server)
npm run lint    # oxlint
npm test        # vitest
```

The build output directory is `../backend/web`; the Go binary embeds those
assets, so run `npm run build` before `go build ./cmd/dubarr`. `emptyOutDir` is
deliberately off so the embed stub (`backend/web/doc.go`) survives. Set
`DUBARR_WEB_OUT` to redirect the build elsewhere for experiments.

See the [root README](../README.md) for the full project and build order.
