FROM ubuntu:22.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y \
    asterisk \
    ca-certificates \
    gettext-base \
    && rm -rf /var/lib/apt/lists/*

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
