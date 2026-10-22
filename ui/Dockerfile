FROM python:3.11-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        libsndfile1 \
        openssl \
        ffmpeg \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY app/ .

# Outside /app on purpose: docker-compose bind-mounts ./ui/app over /app at
# runtime for live-editing, which would otherwise shadow this script.
COPY docker-entrypoint.sh /docker-entrypoint.sh
RUN chmod +x /docker-entrypoint.sh

EXPOSE 7860 7861
ENTRYPOINT ["/docker-entrypoint.sh"]
