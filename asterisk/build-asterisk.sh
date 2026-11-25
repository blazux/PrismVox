#!/bin/bash
set -euo pipefail

mkdir -p /usr/src/asterisk /usr/src/downloads
cd /usr/src/downloads
fetch() {
    local url="$1" checksum="$2" filename="$3"
    curl --fail --location --retry 3 --silent --show-error "$url" -o "$filename"
    printf '%s  %s\n' "$checksum" "$filename" | sha256sum --check --strict -
}

fetch https://downloads.asterisk.org/pub/telephony/asterisk/releases/asterisk-20.22.0.tar.gz \
    ca4a5cbfa0e8b4fa683ae6aa8302769244b6208f1cc466e6025f89457df418e5 asterisk.tar.gz
tar -xzf asterisk.tar.gz --strip-components=1 -C /usr/src/asterisk

# Keep the open-source AMR and Ogg/Opus extensions supplied by Ubuntu's Asterisk.
# Only their build/format integration is applied; the Asterisk and RTP engines
# remain those of the maintained upstream release.
ubuntu=https://archive.ubuntu.com/ubuntu/pool/universe/a/asterisk
version='20.6.0~dfsg+~cs6.13.40431414'
fetch "$ubuntu/asterisk_${version}.orig-Xamr.tar.xz" \
    ba0e753d9e008ad4d55c112dd0dd628fa3ce57e85f7ca5ff117fdc47e90021d8 amr.tar.xz
fetch "$ubuntu/asterisk_${version}.orig-Xopus.tar.xz" \
    1dc2659ade0eb9207a5d22df188690d1528e74374f1e0dbef4a74d824c90c9cf opus.tar.xz
fetch "$ubuntu/asterisk_${version}-2build5.debian.tar.xz" \
    b1dedb284dc4d8791e2284f2e3402dc08f9c4b5318e54cdd46d44d9f8c27116b packaging.tar.xz
mkdir -p /usr/src/asterisk/Xamr /usr/src/asterisk/Xopus /usr/src/packaging
tar -xf amr.tar.xz --strip-components=1 -C /usr/src/asterisk/Xamr
tar -xf opus.tar.xz --strip-components=1 -C /usr/src/asterisk/Xopus
tar -xf packaging.tar.xz -C /usr/src/packaging
cd /usr/src/asterisk
cp -r Xamr/codecs Xamr/include Xamr/res .
cp -r Xopus/codecs Xopus/formats .
for name in 2011_amr_build_tools 2012_amr_codec 2015_opus; do
    patch --batch --fuzz=0 -p1 < "/usr/src/packaging/debian/patches/$name.patch"
done

cd /usr/src/downloads
fetch https://raw.githubusercontent.com/asterisk/third-party/master/pjproject/2.17/pjproject-2.17.tar.bz2 \
    04b2eb1f0f01aa0ad1945b167171843448a51aa6b7c3e806496d434f13a112b7 pjproject-2.17.tar.bz2
export EXTERNALS_CACHE_DIR=/usr/src/downloads
cd /usr/src/asterisk
./bootstrap.sh
./configure --prefix=/usr --sysconfdir=/etc --localstatedir=/var \
    --libdir=/usr/lib --sbindir=/usr/sbin --disable-asteriskssl \
    --with-pjproject-bundled --with-ssl --with-crypto --with-srtp --with-imap=system
make menuselect.makeopts
menuselect/menuselect --disable BUILD_NATIVE --enable chan_sip \
    --enable chan_alsa --enable chan_mgcp --enable chan_skinny --enable res_monitor \
    --enable app_macro --enable app_voicemail_imap --enable app_voicemail_odbc \
    --enable codec_opus_open_source menuselect.makeopts
make -j"$(nproc)" ASTDATADIR=/usr/share/asterisk
make install samples DESTDIR=/install ASTDATADIR=/usr/share/asterisk
mkdir -p /install/var/lib/asterisk /install/var/log/asterisk /install/run/asterisk
if [ -d /install/var/run ]; then
    cp -a /install/var/run/. /install/run/
    rm -rf /install/var/run
fi
strip --strip-unneeded /install/usr/sbin/asterisk /install/usr/lib/asterisk/modules/*.so
mkdir -p /install/usr/share/doc/asterisk
cp COPYING LICENSE /install/usr/share/doc/asterisk/
cp Xamr/LICENSE /install/usr/share/doc/asterisk/LICENSE-amr
cp Xopus/LICENSE /install/usr/share/doc/asterisk/LICENSE-opus

# Install just the shared-library packages used by the compiled daemon/modules
# in the final stage. Compilers, headers and downloaded sources stay here.
find /install/usr -type f -exec ldd '{}' ';' 2>/dev/null \
    | awk '/=> \// {print $3} /^\// {print $1}' | sort -u > /tmp/asterisk-libraries
while IFS= read -r library; do
    dpkg-query -S "$library" 2>/dev/null \
        || dpkg-query -S "/usr$library" 2>/dev/null
done < /tmp/asterisk-libraries \
    | sed -n 's/: \/.*//p' | sort -u > /runtime-packages.txt
test -s /runtime-packages.txt
