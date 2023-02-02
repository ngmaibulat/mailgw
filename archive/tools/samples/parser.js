const  fs = require('fs');
const simpleParser = require('mailparser').simpleParser;

const url = "http://localhost:3000/filter/md5"

let tmpfile = "/tmp/981B1730-DA9D-4820-9698-C3D43382F64B.1.tmp";
let inp = fs.createReadStream(tmpfile);

async function checkHashList(list, url)
{
    let jsondata = JSON.stringify(list);
    // let jsondata = JSON.stringify(obj, censor(obj));

    let req = {
        method: 'POST',
        headers: {
            'Accept': 'application/json',
            'Content-Type': 'application/json'
        },
        body: jsondata
    };

    let action = "allow";

    let prom = fetch(url, req)
        .then(res => res.json())
        .then(res => action = res?.action)
        .catch(err => action = "allow")
    
    await prom;

    return action;
}


async function getAttachments(inp)
{
    let options = {};
    let res = [];
    let parsed = await simpleParser(inp, options);

    // return parsed;

    parsed.attachments.forEach(item => {
        
        let tmp = {
            contentType: item.contentType,
            filename: item.filename,
            size: item.size,
            md5: item.checksum
        };

        res.push(tmp);
    })

    return res;
}


getAttachments(inp)
    .then(attachments => checkHashList(attachments, url))
    .then(res => console.log(res))
    .catch(err => console.log("check failed"))

