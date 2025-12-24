# Деплой GoPublic на VPS

Инструкция по настройке автоматического деплоя GoPublic на VPS сервер через GitHub Actions.

## Требования

- VPS с Ubuntu 22.04+ (или Debian 11+)
- Минимум 1 GB RAM, 10 GB диска
- Домен, направленный на IP сервера (A-запись)
- GitHub репозиторий с GoPublic

## Шаг 1: Подготовка VPS

### 1.1 Подключение к серверу

```bash
ssh root@your-server-ip
```

### 1.2 Создание пользователя для деплоя

```bash
# Создаём пользователя deploy
adduser --disabled-password --gecos "" deploy

# Добавляем в группу sudo (опционально)
usermod -aG sudo deploy

# Разрешаем sudo без пароля для docker команд
echo "deploy ALL=(ALL) NOPASSWD: /usr/bin/docker, /usr/bin/docker-compose" >> /etc/sudoers.d/deploy
```

### 1.3 Установка Docker

```bash
# Установка Docker (официальный скрипт)
curl -fsSL https://get.docker.com | sh

# Добавляем пользователя deploy в группу docker
usermod -aG docker deploy

# Запуск Docker при старте системы
systemctl enable docker
systemctl start docker

# Проверка
docker --version
```

### 1.4 Настройка Firewall

```bash
# Устанавливаем ufw если не установлен
apt update && apt install -y ufw

# Разрешаем SSH (важно сделать первым!)
ufw allow 22/tcp

# Разрешаем порты GoPublic
ufw allow 80/tcp    # HTTP / ACME challenges
ufw allow 443/tcp   # HTTPS
ufw allow 4443/tcp  # Control Plane (клиентские подключения)

# Включаем firewall
ufw enable

# Проверка
ufw status
```

### 1.5 Настройка SSH ключей

```bash
# Переключаемся на пользователя deploy
su - deploy

# Создаём директорию для SSH
mkdir -p ~/.ssh
chmod 700 ~/.ssh

# Создаём файл authorized_keys
touch ~/.ssh/authorized_keys
chmod 600 ~/.ssh/authorized_keys
```

## Шаг 2: Генерация SSH ключей для GitHub Actions

На вашей **локальной машине** (не на сервере):

```bash
# Генерируем новую пару ключей
ssh-keygen -t ed25519 -C "github-actions-deploy" -f ~/.ssh/gopublic_deploy -N ""

# Выводим публичный ключ (для сервера)
cat ~/.ssh/gopublic_deploy.pub

# Выводим приватный ключ (для GitHub Secrets)
cat ~/.ssh/gopublic_deploy
```

### Добавление публичного ключа на сервер

```bash
# На сервере, от имени deploy:
echo "ВСТАВЬТЕ_ПУБЛИЧНЫЙ_КЛЮЧ_СЮДА" >> ~/.ssh/authorized_keys
```

Или с локальной машины:
```bash
ssh-copy-id -i ~/.ssh/gopublic_deploy.pub deploy@your-server-ip
```

### Проверка подключения

```bash
ssh -i ~/.ssh/gopublic_deploy deploy@your-server-ip
```

## Шаг 3: Подготовка директорий на сервере

```bash
# От имени deploy
su - deploy

# Создаём структуру директорий
mkdir -p ~/gopublic/data

# Переходим в директорию
cd ~/gopublic
```

### Создание .env файла

```bash
cat > ~/gopublic/.env << 'EOF'
# Обязательные для production
DOMAIN_NAME=your-domain.com
EMAIL=your-email@example.com

# Telegram OAuth (получить у @BotFather)
TELEGRAM_BOT_TOKEN=123456789:ABCdefGHIjklMNOpqrsTUVwxyz
TELEGRAM_BOT_NAME=YourBotName

# Название проекта (отображается на landing page)
PROJECT_NAME=GoPublic

# GitHub репозиторий для ссылок на скачивание клиента
GITHUB_REPO=your-username/gopublic

# Опционально: ключи для cookies (генерируются автоматически если не заданы)
# SESSION_HASH_KEY=your-32-byte-hex-key
# SESSION_BLOCK_KEY=your-32-byte-hex-key
EOF
```

### Создание docker-compose.yml

```bash
cat > ~/gopublic/docker-compose.yml << 'EOF'
version: '3.8'

services:
  server:
    image: ghcr.io/YOUR_GITHUB_USERNAME/gopublic:latest
    container_name: gopublic-server
    restart: always
    env_file:
      - .env
    ports:
      - "4443:4443"
      - "80:80"
      - "443:443"
    volumes:
      - ./data:/app/data
    working_dir: /app/data
    entrypoint: ["/app/server"]
EOF
```

**Замените `YOUR_GITHUB_USERNAME` на ваш GitHub username (в нижнем регистре).**

## Шаг 4: Настройка GitHub

### 4.1 Secrets (Settings → Secrets and variables → Actions → Secrets)

| Secret | Значение |
|--------|----------|
| `VPS_HOST` | IP адрес или домен вашего VPS |
| `VPS_USER` | `deploy` |
| `VPS_SSH_KEY` | Содержимое файла `~/.ssh/gopublic_deploy` (приватный ключ) |

### 4.2 Variables (Settings → Secrets and variables → Actions → Variables)

| Variable | Значение |
|----------|----------|
| `SERVER_ADDR` | `your-domain.com:4443` |

### 4.3 Настройка доступа к Container Registry

Для приватного репозитория создайте Personal Access Token:

1. GitHub → Settings → Developer settings → Personal access tokens → Tokens (classic)
2. Generate new token с правами: `read:packages`, `write:packages`
3. На сервере выполните:

```bash
# Логин в GitHub Container Registry
docker login ghcr.io -u YOUR_GITHUB_USERNAME -p YOUR_TOKEN
```

Для публичного репозитория этот шаг не нужен.

## Шаг 5: Первый запуск

### Ручной запуск для проверки

```bash
cd ~/gopublic

# Скачиваем образ
docker compose pull

# Запускаем
docker compose up -d

# Проверяем логи
docker compose logs -f
```

### Проверка работы

```bash
# Статус контейнера
docker compose ps

# Проверка портов
ss -tlnp | grep -E '80|443|4443'

# Проверка HTTPS (после получения сертификата)
curl -I https://your-domain.com
```

## Шаг 6: Автоматический деплой

После настройки GitHub Secrets и Variables, каждый push в `main` будет:

1. Запускать тесты
2. Собирать новый Docker образ
3. Пушить образ в GitHub Container Registry
4. Подключаться к VPS по SSH
5. Выполнять `docker compose pull && docker compose up -d`

### Мониторинг деплоя

- GitHub → Actions → Build and Release
- На сервере: `docker compose logs -f`

## Скачивание клиентов

Клиенты доступны в нескольких местах:

### GitHub Releases (для tagged версий)
```
https://github.com/YOUR_USERNAME/gopublic/releases/latest
```

### Прямые ссылки на последний release
```
# macOS (Apple Silicon)
https://github.com/YOUR_USERNAME/gopublic/releases/latest/download/gopublic-macos-arm64

# macOS (Intel)
https://github.com/YOUR_USERNAME/gopublic/releases/latest/download/gopublic-macos-amd64

# Linux
https://github.com/YOUR_USERNAME/gopublic/releases/latest/download/gopublic-linux-amd64

# Windows
https://github.com/YOUR_USERNAME/gopublic/releases/latest/download/gopublic-windows-amd64.exe
```

## Troubleshooting

### Ошибка "permission denied" при подключении SSH

```bash
# Проверьте права на сервере
chmod 700 ~/.ssh
chmod 600 ~/.ssh/authorized_keys

# Проверьте что ключ добавлен
cat ~/.ssh/authorized_keys
```

### Ошибка "port already in use"

```bash
# Найти процесс на порту
sudo lsof -i :80
sudo lsof -i :443

# Остановить nginx/apache если запущены
sudo systemctl stop nginx apache2
```

### Контейнер не запускается

```bash
# Посмотреть логи
docker compose logs

# Проверить .env файл
cat .env

# Перезапустить с пересозданием
docker compose down
docker compose up -d
```

### Сертификат не получается

```bash
# Проверить DNS
dig your-domain.com

# Проверить что порт 80 доступен
curl http://your-domain.com/.well-known/acme-challenge/test

# Логи Let's Encrypt в контейнере
docker compose logs | grep -i acme
```

## Обновление вручную

Если нужно обновить без CI/CD:

```bash
cd ~/gopublic
docker compose pull
docker compose up -d
docker image prune -f  # Удалить старые образы
```

## Бэкап данных

```bash
# Бэкап базы данных
cp ~/gopublic/data/gopublic.db ~/gopublic/data/gopublic.db.backup

# Или с датой
cp ~/gopublic/data/gopublic.db ~/gopublic/data/gopublic.db.$(date +%Y%m%d)
```
