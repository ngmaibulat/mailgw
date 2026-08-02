'use strict';

const {Model} = require('sequelize');

module.exports = (sequelize, DataTypes) => {
  class v_lookup extends Model {
    /**
     * Helper method for defining associations.
     * This method is not a part of Sequelize lifecycle.
     * The `models/index` file will call this method automatically.
     */
    static associate(models) {
      // define association here
    }
  }

  v_lookup.init({

    txn_uuid: DataTypes.STRING,
    md5: DataTypes.STRING,
    contentType: DataTypes.STRING,
    filename: DataTypes.STRING,
    size: DataTypes.INTEGER,
    action: DataTypes.STRING,

    dt: DataTypes.DATE,
    txn_action: DataTypes.STRING,
    encoding: DataTypes.STRING,
    sender: DataTypes.STRING,
    rcpt_list: DataTypes.STRING,
    rcpt_count_accept: DataTypes.INTEGER,
    rcpt_count_tempfail: DataTypes.INTEGER,
    rcpt_count_reject: DataTypes.INTEGER,
    delay_data_post: DataTypes.DOUBLE,
    data_bytes: DataTypes.INTEGER,
    mime_part_count: DataTypes.INTEGER,
  }, {
    sequelize,
    modelName: 'v_lookup',
    freezeTableName: false
  });


  return v_lookup;
};
