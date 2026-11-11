FROM python:3.13-slim
WORKDIR /app
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
COPY processor/requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY processor/app.py processor/app.py
COPY src/consumers/image_processor/filter_photo.py src/consumers/image_processor/filter_photo.py
USER 65532:65532
CMD ["gunicorn", "--bind", "0.0.0.0:8080", "--workers", "2", "--threads", "2", "--timeout", "30", "processor.app:app"]
