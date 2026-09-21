// Synthetic application: deterministic IDs make ambiguous retries safe.
export class Ledger {
  constructor(state) { this.state = state; }
  async fetch(request) {
    const params = new URL(request.url).searchParams;
    const id = params.get("id");
    if (params.has("sequence")) {
      if (request.method === "PUT") {
        const sequence = await this.state.storage.transaction(async txn => {
          const existing = await txn.get(id);
          if (existing !== undefined) return existing;
          const next = ((await txn.get("sequence")) ?? 0) + 1;
          await txn.put("sequence", next);
          await txn.put(id, next);
          return next;
        });
        if (params.has("delay")) await new Promise(resolve => setTimeout(resolve, Number(params.get("delay"))));
        return Response.json({ id, sequence });
      }
      return Response.json({ id, sequence: (await this.state.storage.get(id)) ?? null });
    }
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
