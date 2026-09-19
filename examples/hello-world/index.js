export class Ledger {
  constructor(state) {
    this.state = state;
  }

  async fetch(request) {
    const id = new URL(request.url).searchParams.get("id");
    if (!id) {
      return Response.json({ error: "id is required" }, { status: 400 });
    }

    if (request.method === "PUT") {
      await this.state.storage.put(id, true);
      return Response.json({ id, stored: true });
    }
    if (request.method === "GET") {
      return Response.json({ id, stored: (await this.state.storage.get(id)) === true });
    }
    return new Response("Method not allowed", {
      status: 405,
      headers: { Allow: "GET, PUT" },
    });
  }
}

export default {
  fetch(request, env) {
    const cell = new URL(request.url).searchParams.get("cell") ?? "ledger";
    return env.LEDGER.get(env.LEDGER.idFromName(cell)).fetch(request);
  },
};
