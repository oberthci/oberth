# oberth.ci

Static marketing site for [oberth.ci](https://oberth.ci), deployed as Cloudflare Workers Static Assets.

## Local preview

```sh
npm install
npm run check
npm run dev
```

## Deployment

Routine production deployment uses the merged `main` commit and the checked-in `wrangler.jsonc`. The Cloudflare custom-domain route owns the apex DNS record and managed TLS certificate; do not create a competing apex CNAME or manually edit the generated Worker DNS record.

```sh
npm run deploy
```

The site is intentionally independent of the embedded Oberth dashboard. The public site contains no operational dashboard data or API routes.
