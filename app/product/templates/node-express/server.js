import express from "express";

const app = express();
app.use(express.json());

app.get("/", (_req, res) => res.json({ message: "hello" }));
app.get("/health", (_req, res) => res.json({ ok: true }));

const port = process.env.PORT || 3000;
app.listen(port, "0.0.0.0", () => console.log(`listening on ${port}`));
