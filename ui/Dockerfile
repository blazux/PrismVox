FROM python:3.11-slim

RUN apt-get update && apt-get upgrade -y && apt-get install -y --no-install-recommends \
        libsndfile1 \
        openssl \
        ffmpeg \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir --upgrade "setuptools>=84.2.1" "wheel>=0.46.2" && \
    pip install --no-cache-dir -r requirements.txt

COPY app/ .

# Keep the entrypoint outside /app so the optional development bind mount
# can replace the console sources without hiding the startup script.
COPY docker-entrypoint.sh /docker-entrypoint.sh
RUN chmod +x /docker-entrypoint.sh

EXPOSE 7860 7861
ENTRYPOINT ["/docker-entrypoint.sh"]
