FROM python:3.12-slim

WORKDIR /app

RUN pip install --no-cache-dir --upgrade pip \
 && pip install --no-cache-dir "fastapi==0.115.0" "uvicorn[standard]==0.30.6"

COPY agent.py /app/agent.py

EXPOSE 80

CMD ["uvicorn", "agent:app", "--host", "0.0.0.0", "--port", "80", "--log-level", "info"]
