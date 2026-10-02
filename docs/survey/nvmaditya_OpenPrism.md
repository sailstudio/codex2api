# Prism (local reverse-engineer)

Local monorepo reverse-engineering [OpenAI Prism](https://prism.openai.com): LaTeX research IDE + agent.

## Layout

- `backend/` — FastAPI API (`/health` in Phase 0)
- `frontend/` — Next.js App Router shell
- `AGENTS.md` — agent hard rules
- `PLAN.md` — phased reverse-engineer plan
- `.env.example` — LLM + path env vars

## Quick start (backend + frontend)

```bash
# Git Bash / WSL / macOS / Linux
./start.sh
```

- API: http://127.0.0.1:8000/health  
- UI: http://127.0.0.1:3000/  
- Logs: `logs/backend.log`, `logs/frontend.log`  
- Ctrl+C stops both processes  

Env overrides: `PRISM_PORT`, `PRISM_FRONTEND_PORT`, `NEXT_PUBLIC_API_URL`.

## Backend (manual)

```powershell
cd C:\Code\prism
uv pip install -e backend --python .venv\Scripts\python.exe
.venv\Scripts\python.exe -m uvicorn app.main:app --app-dir backend --host 127.0.0.1 --port 8000
```

Health: `GET http://127.0.0.1:8000/health`

Tests:

```powershell
C:\Code\prism\.venv\Scripts\python.exe -m pytest C:\Code\prism\backend -q
```

## Frontend (manual)

```powershell
cd C:\Code\prism\frontend
npm install
npm run dev
```

Open http://localhost:3000

## Phase

See `PLAN.md` for reverse-engineer phases (projects, agent, TeX, PDF, skills, …).
