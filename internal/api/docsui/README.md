# Vendored Swagger UI

`swagger-ui-bundle.js` and `swagger-ui.css` come from
[swagger-ui-dist](https://www.npmjs.com/package/swagger-ui-dist) **5.32.12**,
Apache-2.0 (`LICENSE`, `NOTICE` alongside).

They are committed rather than fetched at build time, and served from inside
the binary rather than from a CDN, for the same reason the panel is: an
operator copies one file onto a VPS and everything works. A documentation page
that only renders when the machine can reach the internet is exactly the kind
of dependency this project is meant not to have — and a page that pulls
executable JavaScript from a third party at load time is one more party able
to run code in an authenticated admin's browser.

To update, install the version you want and copy the two files plus `LICENSE`
and `NOTICE` over these, then update the version above:

    npm install swagger-ui-dist@<version> --no-save
    cp node_modules/swagger-ui-dist/{swagger-ui-bundle.js,swagger-ui.css,LICENSE,NOTICE} .

The initializer is not vendored; `docs.go` writes its own so the page loads
`../openapi.yaml` rather than the petstore example the stock one points at.
