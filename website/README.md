# oberth.ci

Static marketing site for [oberth.ci](https://oberth.ci), deployed as Cloudflare Workers Static Assets.

## Local preview

```sh
npm install
npm run check
npm run dev
```

## Deployment

Production deployment is part of every Oberth release: an annotated `vX.Y.Z` tag on published `main` runs `.oberth/release.yaml`, whose `release-website` leaf deploys this directory's committed `wrangler.jsonc` and `public/` to Worker `oberth-ci`, and whose tokenless `release-website-readback` leaf proves `https://oberth.ci/`, `https://www.oberth.ci/` and the served scripts byte-match the tag. Node is sha256-pinned (`.oberth/pins/node.*`), wrangler comes from `package-lock.json` (installed offline, sha512-verified, lifecycle scripts disabled), and the only credential is the scoped deploy token at `oberth/data/release/cloudflare-oberth-workers-token` (Terraform output `oberth_website_deploy_token` in `oberthci/terraform`), delivered memory-only by `oberth secretstore exec`. The release refuses a `wrangler.jsonc` that is not the reviewed Worker (name, custom domains, no workers.dev/preview URLs, assets only); `TestWranglerConfigIsTheReviewedPublication` holds the same line on every branch.

The Cloudflare custom-domain route owns the apex DNS record and managed TLS certificate; do not create a competing apex CNAME or manually edit the generated Worker DNS record. Do not deploy by hand with `npm run deploy`: a manual upload bypasses the tag's source binding and the public readback.

The site is intentionally independent of the embedded Oberth dashboard. The public site contains no operational dashboard data or API routes.
