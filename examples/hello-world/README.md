# Hello world

This small celld application stores a named ID in a Durable Object. The `cell` query parameter chooses the object; `id` chooses a value inside it. `GET` returns `{"id":"hello","stored":false}` before the first write. `PUT` stores `true` and returns `{"id":"hello","stored":true}`. A later `GET` returns the same `stored:true` result.

From the repository root, follow the [first application guide](../../site/src/content/docs/start/first-application.md) to deploy this Wrangler project to your dedicated fleet bucket and send a request through port 8080. The example does not retry writes whose outcome is uncertain.
