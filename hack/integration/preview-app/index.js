export class Counter {
  constructor(ctx) { this.ctx = ctx; }
  async fetch(request) {
    const storage = this.ctx.storage;
    storage.sql.exec("CREATE TABLE IF NOT EXISTS items (value INTEGER NOT NULL)");
    if (request.method === "POST") {
      const count = (await storage.get("count") || 0) + 1;
      await storage.put("count", count);
      storage.sql.exec("INSERT INTO items VALUES (?)", count);
      await storage.setAlarm(Number(new URL(request.url).searchParams.get("alarm")));
    }
    return Response.json({ count: await storage.get("count") || 0,
      rows: storage.sql.exec("SELECT COUNT(*) AS n FROM items").one().n,
      alarm: await storage.getAlarm() });
  }
  async alarm() {}
}
export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.pathname === "/") return Response.json({ version: "v1" });
    const name = url.pathname.split("/").pop();
    const id = env.COUNTER.idFromName(name);
    if (url.pathname.startsWith("/id/")) return Response.json({ id: id.toString() });
    return env.COUNTER.get(id).fetch(request);
  }
};
