FROM ubuntu:24.04 AS build

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get upgrade -y && apt-get install -y --no-install-recommends \
    autoconf automake build-essential bison bzip2 ca-certificates curl flex \
    gettext-base libtool pkg-config python3 xz-utils patch \
    libasound2-dev libcap-dev libcodec2-dev libcurl4-openssl-dev libedit-dev \
    libfftw3-dev libgmime-3.0-dev libgsm1-dev libical-dev libiksemel-dev \
    libjack-jackd2-dev libjansson-dev libjwt-dev libldap2-dev liblua5.1-0-dev \
    libneon27-dev libnewt-dev libogg-dev libopencore-amrnb-dev libopencore-amrwb-dev \
    libopus-dev libopusenc-dev libopusfile-dev libpopt-dev libpq-dev libradcli-dev \
    libresample1-dev libsndfile1-dev libsnmp-dev libspandsp-dev libspeex-dev \
    libspeexdsp-dev libsqlite3-dev libsrtp2-dev libssl-dev libunbound-dev \
    liburiparser-dev libvo-amrwbenc-dev libvorbis-dev libxml2-dev libxslt1-dev \
    portaudio19-dev unixodbc-dev uuid-dev zlib1g-dev freetds-dev libc-client2007e-dev \
    && rm -rf /var/lib/apt/lists/*

COPY build-asterisk.sh /usr/local/bin/build-asterisk
RUN /bin/bash /usr/local/bin/build-asterisk

FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive

COPY --from=build /runtime-packages.txt /tmp/runtime-packages.txt
RUN apt-get update && apt-get upgrade -y \
    && apt-get install -y --no-install-recommends ca-certificates gettext-base \
       $(cat /tmp/runtime-packages.txt) \
    && rm -rf /var/lib/apt/lists/* /tmp/runtime-packages.txt
COPY --from=build /install/ /
RUN ldconfig && asterisk -V

COPY etc/asterisk.conf   /etc/asterisk-tpl/asterisk.conf
COPY etc/http.conf       /etc/asterisk-tpl/http.conf
COPY etc/rtp.conf        /etc/asterisk-tpl/rtp.conf
COPY etc/extensions.conf /etc/asterisk-tpl/extensions.conf
COPY etc/modules.conf    /etc/asterisk-tpl/modules.conf
COPY etc/ari.conf.tpl          /etc/asterisk-tpl/ari.conf.tpl
COPY etc/pjsip.conf.tpl        /etc/asterisk-tpl/pjsip.conf.tpl
COPY etc/pjsip_trunk.conf.tpl  /etc/asterisk-tpl/pjsip_trunk.conf.tpl

COPY docker-entrypoint.sh /docker-entrypoint.sh
RUN chmod +x /docker-entrypoint.sh

EXPOSE 5060/udp 8088
ENTRYPOINT ["/docker-entrypoint.sh"]
CMD ["asterisk", "-f", "-vvv"]
