export class Ledger {
  constructor(state) { this.state = state; }
  async fetch(request) {
    const url = new URL(request.url);
    const key = url.searchParams.get("key") || "value";
    if (request.method === "PUT") {
      await this.state.storage.put(key, await request.text());
      return Response.json({ stored: true });
    }
    return Response.json({ value: await this.state.storage.get(key) ?? null });
  }
}
export default {
  fetch(request, env) {
    const cell = new URL(request.url).searchParams.get("cell");
    if (!cell) return Response.json({ version: "density-v1" });
    return env.LEDGER.get(env.LEDGER.idFromName(cell)).fetch(request);
  }
};
