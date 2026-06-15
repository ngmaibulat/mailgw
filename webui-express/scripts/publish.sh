export VER=`npm pkg get version | sed 's/"//g'`
export name="webui-$VER"
export filelist="./db ./public ./src ./templates *.mjs example.env package.json"
export os=`uname`

if [[ "$os" == "Darwin" ]]; then
    alias tar=gtar
fi

shopt -s expand_aliases

rm -fr ../*.gz

tar -cf ../dist.tar --transform "s,^,$name/," $filelist

gzip ../dist.tar

mv ../dist.tar.gz ../$name.tgz

node scripts/s3-upload.mjs ../$name.tgz
