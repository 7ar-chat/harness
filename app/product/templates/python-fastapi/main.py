from fastapi import FastAPI

app = FastAPI(title="API")


@app.get("/")
def root():
    return {"message": "hello"}


@app.get("/health")
def health():
    return {"ok": True}
