# winboat

## 개요

Windows 컨테이너에서 실행되는 **카카오톡을 브라우저로 사용**하는 웹 게이트웨이입니다.
RDP RemoteApp 을 Apache Guacamole 프로토콜로 브라우저에 중계한다.

```
브라우저 ──WebSocket──▶ winboat-web ──Guacamole──▶ guacd ──RDP──▶ WinBoat (Windows)
(127.0.0.1:WEB_PORT)     (내부 8000)               (4822)          (KakaoTalk.exe)
```

주요 기능:

- 페이지를 열면 서버가 HTML에 토큰을 넣어 주므로 별도 로그인 없이 바로 연결
- 브라우저 창 크기에 맞춰 원격 화면 크기 설정
- 원격 화면에서 `Ctrl+V`를 누르면 브라우저 클립보드의 텍스트(한글 포함)를 Windows 클립보드에 넣고 붙여넣음
- 동시에 한 브라우저만 연결 가능

### 디렉터리 구조

```
.
├── docker-compose.yml   # windows + guacd + web
├── .env                 # 실행 설정 (아래 참고)
├── Dockerfile
├── main.go          # 게이트웨이 서버
└── static/          # index.html(템플릿), app.js, style.css, guacamole.js
```

## 이미지 빌드

이 폴더에서 `winboat-web:latest` 이미지를 만든다.

```bash
docker build -t winboat-web:latest .
```

- 빌드 이미지: `golang:1.27-alpine`, 실행 이미지: `alpine:3.22`
- 정적 파일(`static/`)도 바이너리에 포함되므로, HTML/JS/CSS만 바꿔도 다시 빌드해야 합니다.

## winboat 실행

### 1. `.env` 준비


```env
WINDOWS_USERNAME=<Windows 사용자>
WINDOWS_PASSWORD=<Windows 비밀번호>
GUACD_ADDR=guacd:4822
RDP_HOST=windows
WINBOAT_HEALTH_URL=http://windows:7148/health
WEB_PORT=8080
GUACD_INSTRUCTIONS='4.size,%s,%s,2.96;5.audio;...;7.connect,13.VERSION_1_5_0,...;'
```

| 변수 | 설명 |
|---|---|
| `WINDOWS_USERNAME` / `WINDOWS_PASSWORD` | Windows 계정. web에는 `WINBOAT_USERNAME` / `WINBOAT_PASSWORD`로 전달됨 |
| `GUACD_ADDR` | guacd 주소 (compose 서비스 이름 기준) |
| `RDP_HOST` | Windows 컨테이너 호스트 이름 |
| `WINBOAT_HEALTH_URL` | Windows 준비 확인 주소. 연결 전에 최대 180초 기다림 |
| `WEB_PORT` | 브라우저로 접속할 **호스트 포트**. 컨테이너 내부 포트는 8000 고정 |
| `GUACD_INSTRUCTIONS` | guacd에 보내는 handshake 문장 전체 (아래 참고) |

### 2. 실행

WinBoat 앱이 관리하는 `~/.winboat`에서 실행합니다. `~/.winboat/docker-compose.yml`의 `web` 서비스는 이 저장소의 `docker-compose.yml` 으로 교체한다.

### 3. 접속

```bash

1. 로그의 주소(`http://127.0.0.1:<WEB_PORT>/`)를 브라우저로 엽니다.
2. `연결`을 누르면 Windows가 준비될 때까지 기다린 뒤 카카오톡 화면이 나타납니다.
3. 원격 화면을 클릭한 뒤 조작합니다. 텍스트를 붙여넣으려면 `Ctrl+V`를 누릅니다.
(이건 확실하지 않음)

### 코드 수정 후 반영

```bash
docker build -t winboat-web:latest .
docker compose up -d --no-deps web
```

브라우저에서는 `Ctrl+Shift+R`로 새로고침합니다.
