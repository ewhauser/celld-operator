// Synthetic application: deterministic IDs make ambiguous retries safe.
export class Ledger {
  constructor(state) { this.state = state; }
  async fetch(request) {
    const id = new URL(request.url).searchParams.get("id");
    if (request.method === "PUT") {
      await this.state.storage.put(id, true);
      return Response.json({ id, stored: true });
    }
    return Response.json({ id, stored: (await this.state.storage.get(id)) === true });
  }
}
export default {
  fetch(request, env) {
    return env.LEDGER.get(env.LEDGER.idFromName(new URL(request.url).searchParams.get("cell") ?? "ledger")).fetch(request);
  }
};
