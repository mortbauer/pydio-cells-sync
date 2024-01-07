FROM node:21.5.0-bullseye-slim as frontend

WORKDIR /app

COPY app/ux .

RUN npm install && npm run build

FROM golang:1.20 as build
WORKDIR /app
RUN apt update && apt install -y --no-install-recommends libwebkit2gtk-4.0-37 libwebkit2gtk-4.0-dev libgtk-3-dev libayatana-appindicator3-dev libappindicator3-dev musl-dev musl-tools linux-headers-generic
COPY --from=frontend /app/build /app/app/ux/build
COPY app app
COPY cmd cmd
COPY common common
COPY config config
COPY control control
COPY endpoint endpoint
COPY i18n i18n
COPY libs libs
COPY tests tests
COPY go.mod go.sum main.go Makefile .
# FROM build as tst
RUN make dist
# RUN CGO_ENABLED=1 CC=musl-gcc go build \
#     -ldflags '-linkmode external -extldflags=-static' \
# 	-ldflags "-X github.com/pydio/cells-sync/common.Version=${CELLS_VERSION}" \
#     -o cells-sync .

COPY cells-sync.desktop AppRun.sh ./

RUN wget https://github.com/linuxdeploy/linuxdeploy/releases/download/continuous/linuxdeploy-x86_64.AppImage \
    && chmod +x linuxdeploy-x86_64.AppImage \
    && ./linuxdeploy-x86_64.AppImage --appimage-extract

RUN apt install -y file
COPY Test.svg cells-sync.svg
RUN ./squashfs-root/AppRun --appdir AppDir -e cells-sync --custom-apprun AppRun.sh -i cells-sync.svg --create-desktop-file --output appimage

FROM scratch AS artifact
COPY --from=build /app/cells-sync .
COPY --from=build /app/AppDir AppDir
